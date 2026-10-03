package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func uncertainExecutionError() error {
	return fmt.Errorf("wrapped: %w", cliproxyexecutor.MarkExecutionUncertain(customStatusError{code: http.StatusServiceUnavailable, msg: "ambiguous transport failure"}))
}

func uncertainTestManager(t *testing.T, action string) (*Manager, []string) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, 0, 6)
	m.SetConfig(&internalconfig.Config{OAuthRequestScopedErrors: map[string][]internalconfig.RequestScopedErrorRule{
		"codex": {{Status: http.StatusServiceUnavailable, Match: []string{"ambiguous transport failure"}, Action: action}},
	}})
	return m, registerOverloadAuths(t, m, 2)
}

func assertUncertainAuthsAvailable(t *testing.T, m *Manager, ids []string) {
	t.Helper()
	for _, id := range ids {
		auth, ok := m.GetByID(id)
		if !ok {
			t.Fatalf("missing auth %s", id)
		}
		if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded {
			t.Fatalf("ambiguous error cooled credential %s", id)
		}
		for model, state := range auth.ModelStates {
			if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
				t.Fatalf("ambiguous error cooled model %s", model)
			}
		}
	}
}

func TestExecutionUncertainStopsConfiguredRetries(t *testing.T) {
	for _, action := range []string{RequestScopedActionContinue, RequestScopedActionContinueAndCooldown} {
		t.Run(action, func(t *testing.T) {
			m, ids := uncertainTestManager(t, action)
			calls := 0
			cause := uncertainExecutionError()
			m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return cliproxyexecutor.Response{}, cause
			}})
			_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			if !errors.Is(err, cause) || !cliproxyexecutor.IsExecutionUncertain(err) || statusCodeFromError(err) != http.StatusServiceUnavailable {
				t.Fatalf("lost underlying error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("executed %d requests, want 1", calls)
			}
			assertUncertainAuthsAvailable(t, m, ids)
			if _, retry := m.shouldRetryAfterError(err, 0, []string{"codex"}, "gpt-5.6-terra", time.Second); retry {
				t.Fatal("retry policy replayed uncertain execution")
			}
		})
	}
}

func TestExecutionUncertainStreamStopsBeforeAndAfterData(t *testing.T) {
	for _, stage := range []string{"initial error", "before data", "after data"} {
		t.Run(stage, func(t *testing.T) {
			m, ids := uncertainTestManager(t, RequestScopedActionContinueAndCooldown)
			var calls atomic.Int32
			cause := uncertainExecutionError()
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				calls.Add(1)
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				if stage == "initial error" {
					return nil, cause
				}
				ch := make(chan cliproxyexecutor.StreamChunk, 2)
				if stage == "after data" {
					ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: generated\n\n")}
				}
				ch <- cliproxyexecutor.StreamChunk{Err: cause}
				close(ch)
				return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Test": {"preserved"}}, Chunks: ch}, nil
			}})
			result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			data := 0
			if result != nil {
				if result.Headers.Get("X-Test") != "preserved" {
					t.Fatal("stream headers were lost")
				}
				for chunk := range result.Chunks {
					if len(chunk.Payload) > 0 {
						data++
					}
					if chunk.Err != nil {
						err = chunk.Err
					}
				}
			}
			if !errors.Is(err, cause) || !cliproxyexecutor.IsExecutionUncertain(err) || statusCodeFromError(err) != http.StatusServiceUnavailable {
				t.Fatalf("lost stream error: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("executed %d requests, want 1", calls.Load())
			}
			if stage == "after data" && data != 1 {
				t.Fatalf("forwarded %d data chunks, want 1", data)
			}
			assertUncertainAuthsAvailable(t, m, ids)
		})
	}
}

func TestExecutionUncertainStreamStopsModelPool(t *testing.T) {
	m, ids := uncertainTestManager(t, RequestScopedActionContinue)
	auth, _ := m.GetByID(ids[0])
	for _, stage := range []string{"initial error", "before data"} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			executor := &customStreamMockExecutor{identifier: "codex", streamFn: func(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				calls++
				if stage == "initial error" {
					return nil, uncertainExecutionError()
				}
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				ch <- cliproxyexecutor.StreamChunk{Err: uncertainExecutionError()}
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			}}
			_, err := m.executeStreamWithModelPool(context.Background(), executor, auth, "codex", cliproxyexecutor.Request{Model: "alias"}, cliproxyexecutor.Options{}, "alias", "", []string{"model-a", "model-b"}, true, OAuthModelAliasResult{}, nil, true, false)
			if !cliproxyexecutor.IsExecutionUncertain(err) || calls != 1 {
				t.Fatalf("model pool calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestExecutionUncertainCancellationStops(t *testing.T) {
	m, _ := uncertainTestManager(t, RequestScopedActionContinue)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		cancel()
		return cliproxyexecutor.Response{}, uncertainExecutionError()
	}})
	_, err := m.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestExecutionUncertainStopsNonstreamModelPool(t *testing.T) {
	for _, countTokens := range []bool{false, true} {
		t.Run(fmt.Sprintf("count=%t", countTokens), func(t *testing.T) {
			cause := uncertainExecutionError()
			executor := &openAICompatPoolExecutor{
				id:            openAICompatPoolProviderKey,
				executeErrors: map[string]error{"model-a": cause, "model-b": cause},
				countErrors:   map[string]error{"model-a": cause, "model-b": cause},
			}
			m := newOpenAICompatPoolTestManager(t, "alias", []internalconfig.OpenAICompatibilityModel{
				{Name: "model-a", Alias: "alias"}, {Name: "model-b", Alias: "alias"},
			}, executor)
			m.SetRetryConfig(5, 0, 6)
			req := cliproxyexecutor.Request{Model: "alias"}
			var err error
			var calls []string
			if countTokens {
				_, err = m.ExecuteCount(context.Background(), []string{openAICompatPoolProviderKey}, req, cliproxyexecutor.Options{})
				calls = executor.CountModels()
			} else {
				_, err = m.Execute(context.Background(), []string{openAICompatPoolProviderKey}, req, cliproxyexecutor.Options{})
				calls = executor.ExecuteModels()
			}
			if !errors.Is(err, cause) || len(calls) != 1 {
				t.Fatalf("model pool calls=%v error=%v", calls, err)
			}
		})
	}
}

func TestExecutionUncertainStreamCancellationStops(t *testing.T) {
	m, _ := uncertainTestManager(t, RequestScopedActionContinue)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls++
		cancel()
		return nil, uncertainExecutionError()
	}})
	_, err := m.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestExecutionUncertainUnmarkedFailoverUnchanged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, ids := uncertainTestManager(t, RequestScopedActionContinue)
			calls := 0
			failure := customStatusError{code: http.StatusServiceUnavailable, msg: "ambiguous transport failure"}
			if stream {
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					calls++
					if auth.ID == ids[0] {
						return nil, failure
					}
					return successStreamResult(), nil
				}})
				result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					calls++
					if auth.ID == ids[0] {
						return cliproxyexecutor.Response{}, failure
					}
					return cliproxyexecutor.Response{}, nil
				}})
				if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{}); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatalf("executed %d requests, want 2", calls)
			}
		})
	}
}
