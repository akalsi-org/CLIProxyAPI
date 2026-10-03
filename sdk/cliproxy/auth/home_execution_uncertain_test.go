package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type uncertainHomeDispatcher struct {
	accountedHomeExecutionDispatcher
	calls int
}

func (d *uncertainHomeDispatcher) RPopAuth(ctx context.Context, model, session string, headers http.Header, count int) ([]byte, error) {
	d.calls++
	return d.accountedHomeExecutionDispatcher.RPopAuth(ctx, model, session, headers, count)
}

func TestHomeExecutionUncertainStopsAndReleases(t *testing.T) {
	for _, countTokens := range []bool{false, true} {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized} {
			t.Run(fmt.Sprintf("count=%t/status=%d", countTokens, status), func(t *testing.T) {
				hook := &recordingHook{}
				manager := NewManager(nil, nil, hook)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(5, 0, 6)
				dispatcher := &uncertainHomeDispatcher{accountedHomeExecutionDispatcher: accountedHomeExecutionDispatcher{auths: []Auth{
					{ID: "uncertain-home-1", Provider: "home-execution", Status: StatusActive, Metadata: map[string]any{
						"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{Status: status, Match: []string{"ambiguous execution"}, Action: RequestScopedActionContinueAndCooldown}},
					}},
					{ID: "uncertain-home-2", Provider: "home-execution", Status: StatusActive},
				}}}
				registry := executionregistry.New()
				releases := make(chan executionregistry.ReleaseGroup, 2)
				registry.SetReleaseSink(func(group executionregistry.ReleaseGroup, _ int64) { releases <- group })
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				cause := customStatusError{code: status, msg: "ambiguous execution"}
				marked := fmt.Errorf("wrapped: %w", cliproxyexecutor.MarkExecutionUncertain(cause))
				calls := 0
				var attemptCtx context.Context
				execute := func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					calls++
					attemptCtx = ctx
					cliproxyexecutor.MarkUpstreamAttempt(ctx)
					return cliproxyexecutor.Response{}, marked
				}
				manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "home-execution", executeFn: execute, countFn: execute})
				var err error
				if countTokens {
					_, err = manager.ExecuteCount(context.Background(), []string{"home-execution"}, cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{})
				} else {
					_, err = manager.Execute(context.Background(), []string{"home-execution"}, cliproxyexecutor.Request{Model: "model-a"}, cliproxyexecutor.Options{})
				}
				if !errors.Is(err, marked) || !cliproxyexecutor.IsExecutionUncertain(err) || statusCodeFromError(err) != status {
					t.Fatalf("lost uncertain error: %v", err)
				}
				if calls != 1 || dispatcher.calls != 1 {
					t.Fatalf("executor=%d dispatch=%d, want one each", calls, dispatcher.calls)
				}
				if attemptCtx == nil || !errors.Is(attemptCtx.Err(), context.Canceled) {
					t.Fatal("selection did not end its attempt context")
				}
				result := hook.lastResult.Load()
				if result == nil || result.Error == nil || result.Error.Code != ErrorCodeRequestScoped || result.CredentialScope || result.RetryAfter != nil {
					t.Fatalf("uncertain Home result changed availability: %+v", result)
				}
				select {
				case group := <-releases:
					if group != (executionregistry.ReleaseGroup{CredentialID: "uncertain-home-1", Model: "model-a"}) {
						t.Fatalf("wrong release group: %+v", group)
					}
				default:
					t.Fatal("selection did not release its accounted resources")
				}
				select {
				case group := <-releases:
					t.Fatalf("duplicate release: %+v", group)
				default:
				}
			})
		}
	}
}
