package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const antigravityCoolingBareBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestNewAntigravityStatusErrCoolingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		legacyWide bool
		optInWide  bool
		legacyWait time.Duration
		optInWait  time.Duration
	}{
		{"bare resource", antigravityCoolingBareBody, true, false, 5 * time.Minute, 5 * time.Minute},
		{"explicit quota", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`, true, true, 5 * time.Minute, 5 * time.Minute},
		{"text quota", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Quota exhausted"}}`, true, true, 5 * time.Minute, 5 * time.Minute},
		{"explicit credits balance", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"INSUFFICIENT_G1_CREDITS_BALANCE"}]}}`, true, true, 5 * time.Minute, 5 * time.Minute},
		{"short rate", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`, false, false, 500 * time.Millisecond, 500 * time.Millisecond},
		{"medium rate", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`, false, false, 30 * time.Second, 30 * time.Second},
		{"long rate with model", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.8-flash-high"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`, true, false, time.Hour, time.Hour},
		{"long rate without model", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`, true, false, time.Hour, time.Hour},
		{"no hint rate", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`, false, false, 0, 5 * time.Minute},
		{"generic long hint", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`, false, false, time.Hour, time.Hour},
		{"long explicit quota", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`, true, true, time.Hour, time.Hour},
		{"quota after rate", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`, false, true, 500 * time.Millisecond, 500 * time.Millisecond},
		{"unstructured", `{"error":{"message":"too many requests"}}`, false, false, 0, 0},
	} {
		for _, policy := range []string{"missing", "false", "true"} {
			t.Run(tc.name+"/"+policy, func(t *testing.T) {
				var cfg *config.Config
				if policy != "missing" {
					cfg = &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: policy == "true"}}
				}
				wantWide, wantWait := tc.legacyWide, tc.legacyWait
				if policy == "true" {
					wantWide, wantWait = tc.optInWide, tc.optInWait
				}
				err := newAntigravityStatusErr(cfg, http.StatusTooManyRequests, []byte(tc.body))
				if err.IsCredentialScoped() != wantWide {
					t.Fatalf("IsCredentialScoped = %v, want %v", err.IsCredentialScoped(), wantWide)
				}
				if wantWait == 0 {
					if err.RetryAfter() != nil {
						t.Fatalf("RetryAfter = %v, want nil", err.RetryAfter())
					}
				} else if err.RetryAfter() == nil || *err.RetryAfter() != wantWait {
					t.Fatalf("RetryAfter = %v, want %s", err.RetryAfter(), wantWait)
				}
			})
		}
	}
	cfg := &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: true}}
	err := newAntigravityStatusErr(cfg, http.StatusForbidden, []byte(antigravityCoolingBareBody))
	if err.IsCredentialScoped() || err.RetryAfter() != nil || err.StatusCode() != http.StatusForbidden {
		t.Fatalf("non-429 behavior changed: %#v", err)
	}
}

func antigravityCoolingTestAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: id, Provider: "antigravity", Status: cliproxyauth.StatusActive,
		Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth, "base_url": baseURL},
		Metadata:   map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}
}

func newAntigravityCoolingTestManager(t *testing.T, cfg *config.Config, auth *cliproxyauth.Auth) *cliproxyauth.Manager {
	t.Helper()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 1)
	manager.RegisterExecutor(NewAntigravityExecutor(cfg))
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models := []*registry.ModelInfo{
		{ID: "gemini-3.8-flash-high", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
		{ID: "gemini-3.8-pro-high"},
		{ID: "gemini-3.8-pro-low"},
		{ID: "cooling-alias", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{
		"antigravity": {{Name: "gemini-3.8-flash-high", Alias: "cooling-alias", Fork: true}},
	})
	manager.RefreshSchedulerEntry(auth.ID)
	return manager
}

func executeAntigravityCoolingTest(manager *cliproxyauth.Manager, model string, stream bool) error {
	request := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic diagnostic"}]}]}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}
	if !stream {
		_, err := manager.Execute(context.Background(), []string{"antigravity"}, request, opts)
		return err
	}
	result, err := manager.ExecuteStream(context.Background(), []string{"antigravity"}, request, opts)
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

func TestAntigravityCoolingManagerIsolation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/stream=%v", enabled, stream), func(t *testing.T) {
				var calls atomic.Int32
				var failed atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, _ := io.ReadAll(r.Body)
					if gjson.GetBytes(body, "model").String() == "gemini-3.8-flash-high" && !failed.Swap(true) {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, antigravityCoolingBareBody)
						return
					}
					response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", response)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, response)
					}
				}))
				t.Cleanup(server.Close)
				cfg := &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: enabled}}
				auth := antigravityCoolingTestAuth(fmt.Sprintf("cooling-isolation-%v-%v", enabled, stream), server.URL)
				manager := newAntigravityCoolingTestManager(t, cfg, auth)
				// B already has model state; C is registered but has never run.
				manager.MarkResult(context.Background(), cliproxyauth.Result{AuthID: auth.ID, Provider: auth.Provider, Model: "gemini-3.8-pro-high", Success: true})
				if err := executeAntigravityCoolingTest(manager, "cooling-alias(high)", stream); err == nil {
					t.Fatal("model A must return its upstream 429")
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("initial upstream calls = %d, want 1", got)
				}
				snapshot, _ := manager.GetByID(auth.ID)
				state := snapshot.ModelStates["gemini-3.8-flash-high"]
				if state == nil || !state.Unavailable || state.NextRetryAfter.Sub(state.UpdatedAt) != 5*time.Minute {
					t.Fatalf("canonical model A must retain the five-minute hold: %#v", state)
				}
				credentialScoped := snapshot.Quota.Reason == "credential_quota"
				if credentialScoped != !enabled {
					t.Fatalf("credential quota scope does not match policy: %#v", snapshot.Quota)
				}
				for _, model := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-high(low)", "cooling-alias", "cooling-alias(low)"} {
					if err := executeAntigravityCoolingTest(manager, model, stream); err == nil {
						t.Fatalf("%s bypassed model A's hold", model)
					}
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("blocked model A made upstream calls: %d", got)
				}
				for _, model := range []string{"gemini-3.8-pro-high", "gemini-3.8-pro-low"} {
					err := executeAntigravityCoolingTest(manager, model, stream)
					if enabled && err != nil {
						t.Fatalf("sibling %s was blocked: %v", model, err)
					}
					if !enabled && err == nil {
						t.Fatalf("legacy credential hold allowed sibling %s", model)
					}
				}
				wantCalls := int32(1)
				if enabled {
					wantCalls = 3
				}
				if calls.Load() != wantCalls {
					t.Fatalf("upstream calls = %d, want %d", calls.Load(), wantCalls)
				}
				// Rebuild scheduler state through the public configuration path.
				manager.SetConfig(&config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: true}})
				manager.RefreshSchedulerEntry(auth.ID)
				if err := executeAntigravityCoolingTest(manager, "gemini-3.8-flash-high", stream); err == nil || calls.Load() != wantCalls {
					t.Fatal("configuration update cleared an active hold")
				}
				if !enabled {
					if err := executeAntigravityCoolingTest(manager, "gemini-3.8-pro-low", stream); err == nil || calls.Load() != wantCalls {
						t.Fatal("enabling model cooling cleared the prior credential-wide hold")
					}
					return
				}
				// Move retained deadlines into the past. No wall-clock wait is required.
				snapshot, _ = manager.GetByID(auth.ID)
				past := time.Unix(1, 0)
				snapshot.ModelStates["gemini-3.8-flash-high"].NextRetryAfter = past
				snapshot.ModelStates["gemini-3.8-flash-high"].Quota.NextRecoverAt = past
				snapshot.NextRetryAfter = past
				snapshot.Quota.NextRecoverAt = past
				if _, err := manager.Update(context.Background(), snapshot); err != nil {
					t.Fatal(err)
				}
				if err := executeAntigravityCoolingTest(manager, "cooling-alias(low)", stream); err != nil {
					t.Fatalf("expired model hold did not allow model A: %v", err)
				}
				if calls.Load() != wantCalls+1 {
					t.Fatal("expired model A did not make exactly one upstream call")
				}
			})
		}
	}
}

type antigravityCoolingResultHook struct {
	cliproxyauth.NoopHook
	mu      sync.Mutex
	results []cliproxyauth.Result
}

func (h *antigravityCoolingResultHook) OnResult(_ context.Context, result cliproxyauth.Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, result)
}

func TestAntigravityCoolingHomeOwnsState(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/stream=%v", enabled, stream), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, antigravityCoolingBareBody)
				}))
				t.Cleanup(server.Close)
				cfg := &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: enabled}}
				cfg.Home.Enabled = true
				hook := &antigravityCoolingResultHook{}
				manager := cliproxyauth.NewManager(nil, nil, hook)
				manager.SetConfig(cfg)
				manager.SetRetryConfig(0, 0, 1)
				manager.RegisterExecutor(NewAntigravityExecutor(cfg))
				manager.PublishHomeDispatch(&antigravityHomeModelCapabilityDispatcher{baseURL: server.URL}, executionregistry.New(), 1)
				// Home dispatch uses this ID. Local state must not receive its cooldown.
				local := antigravityCoolingTestAuth("home-antigravity-3.8-auth", "http://synthetic.invalid")
				if _, err := manager.Register(context.Background(), local); err != nil {
					t.Fatal(err)
				}
				before, _ := manager.GetByID(local.ID)
				if err := executeAntigravityCoolingTest(manager, "gemini-3.8-flash-high", stream); err == nil {
					t.Fatal("Home execution must return the upstream 429")
				}
				if calls.Load() != 1 {
					t.Fatalf("Home upstream calls = %d, want 1", calls.Load())
				}
				after, _ := manager.GetByID(local.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("Home-owned failure changed the same-ID local auth")
				}
				hook.mu.Lock()
				defer hook.mu.Unlock()
				if len(hook.results) != 1 {
					t.Fatalf("Home result observations = %d, want 1", len(hook.results))
				}
				result := hook.results[0]
				if result.CredentialScope != !enabled || result.RetryAfter == nil || *result.RetryAfter != 5*time.Minute {
					t.Fatalf("Home observation lost cooling policy: %#v", result)
				}
			})
		}
	}
}

func TestAntigravityCoolingClaudeExecuteUsesPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, antigravityCoolingBareBody)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: true}}
	executor := NewAntigravityExecutor(cfg)
	_, err := executor.Execute(context.Background(), antigravityCoolingTestAuth("cooling-claude", server.URL), cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic diagnostic"}]}]}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity})
	status, ok := err.(statusErr)
	if !ok || status.IsCredentialScoped() || status.RetryAfter() == nil || *status.RetryAfter() != 5*time.Minute {
		t.Fatalf("non-Gemini Execute did not apply model cooling: %v", err)
	}
}

func TestAntigravityCoolingConcurrentFailuresDoNotShortenReset(t *testing.T) {
	cfg := &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: true}}
	auth := antigravityCoolingTestAuth("cooling-concurrent", "http://synthetic.invalid")
	manager := newAntigravityCoolingTestManager(t, cfg, auth)
	body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`)
	err := newAntigravityStatusErr(cfg, http.StatusTooManyRequests, body)
	result := cliproxyauth.Result{AuthID: auth.ID, Provider: auth.Provider, Model: "gemini-3.8-flash-high(high)", Error: &cliproxyauth.Error{HTTPStatus: err.StatusCode(), Message: err.Error()}, RetryAfter: err.RetryAfter(), CredentialScope: err.IsCredentialScoped()}
	manager.MarkResult(context.Background(), result)
	snapshot, _ := manager.GetByID(auth.ID)
	deadline := snapshot.ModelStates["gemini-3.8-flash-high"].NextRetryAfter
	if deadline.Sub(snapshot.ModelStates["gemini-3.8-flash-high"].UpdatedAt) != time.Hour {
		t.Fatal("the initial failure lost the provider's one-hour reset")
	}
	shortErr := newAntigravityStatusErr(cfg, http.StatusTooManyRequests, []byte(antigravityCoolingBareBody))
	result.RetryAfter = shortErr.RetryAfter()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.MarkResult(context.Background(), result)
		}()
	}
	wg.Wait()
	snapshot, _ = manager.GetByID(auth.ID)
	state := snapshot.ModelStates["gemini-3.8-flash-high"]
	if state.NextRetryAfter.Before(deadline) || state.Quota.NextRecoverAt.Before(deadline) {
		t.Fatalf("concurrent failures shortened the provider reset: %#v", state)
	}
	for _, model := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-high(low)", "cooling-alias(low)"} {
		if _, err := manager.SelectAuth(context.Background(), auth.Provider, model, cliproxyexecutor.Options{}); err == nil {
			t.Fatalf("long provider reset allowed the failed model through %s", model)
		}
	}
	for _, model := range []string{"gemini-3.8-pro-high", "gemini-3.8-pro-low"} {
		if _, err := manager.SelectAuth(context.Background(), auth.Provider, model, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("model-scoped long reset blocked sibling %s: %v", model, err)
		}
	}
}
