package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const antigravityThrottleTestBare = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestAntigravityQuotaGroupExhaustedUntil(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour).Format(time.RFC3339)
	weekly := now.Add(72 * time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)
	summary := func(gemini, claude string) []byte {
		return []byte(`{"groups":[` +
			`{"displayName":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[` + gemini + `]},` +
			`{"displayName":"Claude and GPT models","description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS","buckets":[` + claude + `]}]}`)
	}
	full := `{"bucketId":"x","remainingFraction":1,"resetTime":"` + reset + `"}`
	for _, tc := range []struct {
		name  string
		body  []byte
		model string
		want  string
	}{
		{"quota remains", summary(full, full), "gemini-3.8-flash-high", ""},
		{"omitted zero fraction is exhausted", summary(`{"bucketId":"gemini-5h","resetTime":"`+reset+`"}`, full), "gemini-pro-agent", reset},
		{"explicit zero fraction", summary(`{"bucketId":"gemini-5h","remainingFraction":0,"resetTime":"`+reset+`"}`, full), "gemini-3.1-pro-low", reset},
		{"latest exhausted reset wins", summary(`{"bucketId":"gemini-5h","resetTime":"`+reset+`"},{"bucketId":"gemini-weekly","resetTime":"`+weekly+`"}`, full), "gemini-3.7-flash-high", weekly},
		{"other group exhausted", summary(full, `{"bucketId":"3p-5h","resetTime":"`+reset+`"}`), "gemini-3.7-flash-high", ""},
		{"claude uses its group", summary(full, `{"bucketId":"3p-5h","resetTime":"`+reset+`"}`), "claude-sonnet-5-5-high", reset},
		{"gpt uses its group", summary(full, `{"bucketId":"3p-5h","resetTime":"`+reset+`"}`), "gpt-oss-120b-medium", reset},
		{"past reset is ignored", summary(`{"bucketId":"gemini-5h","resetTime":"`+past+`"}`, full), "gemini-3.7-flash-high", ""},
		{"missing reset is ignored", summary(`{"bucketId":"gemini-5h"}`, full), "gemini-3.7-flash-high", ""},
		{"malformed body", []byte(`not json`), "gemini-3.7-flash-high", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until, exhausted := antigravityQuotaGroupExhaustedUntil(tc.body, tc.model, now)
			if tc.want == "" {
				if exhausted {
					t.Fatalf("exhausted until %s, want quota remaining", until)
				}
				return
			}
			if !exhausted || until.Format(time.RFC3339) != tc.want {
				t.Fatalf("until = %s exhausted = %v, want %s", until, exhausted, tc.want)
			}
		})
	}
}

type antigravityThrottleTestUpstream struct {
	generation atomic.Int32
	summaries  atomic.Int32
	status     atomic.Int32
	body       atomic.Value
	summary    atomic.Value
}

func newAntigravityThrottleTestUpstream() *antigravityThrottleTestUpstream {
	upstream := &antigravityThrottleTestUpstream{}
	upstream.status.Store(http.StatusTooManyRequests)
	upstream.body.Store(antigravityThrottleTestBare)
	upstream.summary.Store(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","remainingFraction":1,"resetTime":"2999-01-01T00:00:00Z"}]}]}`)
	return upstream
}

func (u *antigravityThrottleTestUpstream) context() context.Context {
	return context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == antigravityQuotaSummaryPath {
			u.summaries.Add(1)
			return issue6199AntigravityJSONResponse(req, u.summary.Load().(string)), nil
		}
		u.generation.Add(1)
		status := int(u.status.Load())
		if status == http.StatusOK {
			return issue6199AntigravityJSONResponse(req, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`), nil
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(u.body.Load().(string))),
			Request:    req,
		}, nil
	}))
}

func antigravityThrottleTestExecute(t *testing.T, ctx context.Context, executor *AntigravityExecutor, auth *cliproxyauth.Auth) statusErr {
	t.Helper()
	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash-high",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity})
	if err == nil {
		return statusErr{}
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr: %v", err, err)
	}
	return status
}

func antigravityThrottleTestAuth(t *testing.T) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{
		"access_token": "synthetic-token",
		"project_id":   "synthetic-project",
		"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
}

func assertAntigravityThrottleDelay(t *testing.T, err statusErr, low, high time.Duration) {
	t.Helper()
	if err.StatusCode() != http.StatusTooManyRequests || err.IsCredentialScoped() {
		t.Fatalf("throttling error = status %d credential scoped %v, want model-scoped 429", err.StatusCode(), err.IsCredentialScoped())
	}
	if err.RetryAfter() == nil || *err.RetryAfter() < low || *err.RetryAfter() > high {
		t.Fatalf("retry after = %v, want %s..%s", err.RetryAfter(), low, high)
	}
}

func TestAntigravityThrottleBacksOffAndResetsOnSuccess(t *testing.T) {
	upstream := newAntigravityThrottleTestUpstream()
	ctx := upstream.context()
	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityThrottleTestAuth(t)
	t.Cleanup(func() {
		resetAntigravityThrottle(auth, "gemini-3.7-flash-high")
		antigravityQuotaSummaryByAuth.Delete(auth.ID)
	})

	assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 8*time.Second, 12*time.Second)
	assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 16*time.Second, 24*time.Second)
	assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 32*time.Second, 48*time.Second)
	for range 4 {
		assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 48*time.Second, time.Minute)
	}
	// The cached quota summary serves the whole burst.
	if got := upstream.summaries.Load(); got != 1 {
		t.Fatalf("quota summary requests = %d, want 1", got)
	}

	upstream.status.Store(http.StatusOK)
	if err := antigravityThrottleTestExecute(t, ctx, executor, auth); err.StatusCode() != 0 {
		t.Fatalf("success returned %v", err)
	}
	upstream.status.Store(http.StatusTooManyRequests)
	assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 8*time.Second, 12*time.Second)
	if got := upstream.generation.Load(); got != 9 {
		t.Fatalf("generation requests = %d, want 9 with no internal retries", got)
	}
}

func TestAntigravityThrottleExhaustedQuotaHoldsUntilReset(t *testing.T) {
	upstream := newAntigravityThrottleTestUpstream()
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	upstream.summary.Store(fmt.Sprintf(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","resetTime":%q}]}]}`, reset.Format(time.RFC3339)))
	ctx := upstream.context()
	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityThrottleTestAuth(t)
	t.Cleanup(func() {
		resetAntigravityThrottle(auth, "gemini-3.7-flash-high")
		antigravityQuotaSummaryByAuth.Delete(auth.ID)
	})

	for range 2 {
		err := antigravityThrottleTestExecute(t, ctx, executor, auth)
		assertAntigravityThrottleDelay(t, err, 2*time.Hour-time.Minute, 2*time.Hour)
	}
	if got := upstream.summaries.Load(); got != 1 {
		t.Fatalf("quota summary requests = %d, want 1", got)
	}
}

func TestAntigravityThrottleFallsBackWhenSummaryFails(t *testing.T) {
	upstream := newAntigravityThrottleTestUpstream()
	upstream.summary.Store(`not json`)
	ctx := upstream.context()
	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityThrottleTestAuth(t)
	t.Cleanup(func() {
		resetAntigravityThrottle(auth, "gemini-3.7-flash-high")
		antigravityQuotaSummaryByAuth.Delete(auth.ID)
	})

	assertAntigravityThrottleDelay(t, antigravityThrottleTestExecute(t, ctx, executor, auth), 8*time.Second, 12*time.Second)
}

func TestAntigravityThrottleKeepsExplicitQuotaHandling(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"quota exhausted", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`},
		{"credits balance", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"INSUFFICIENT_G1_CREDITS_BALANCE"}]}}`},
		{"rate limit with delay", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			upstream := newAntigravityThrottleTestUpstream()
			upstream.body.Store(body)
			ctx := upstream.context()
			executor := NewAntigravityExecutor(&config.Config{})
			auth := antigravityThrottleTestAuth(t)
			t.Cleanup(func() {
				resetAntigravityThrottle(auth, "gemini-3.7-flash-high")
				antigravityQuotaSummaryByAuth.Delete(auth.ID)
			})

			got := antigravityThrottleTestExecute(t, ctx, executor, auth)
			want := newAntigravityStatusErr(executor.cfg, http.StatusTooManyRequests, []byte(body))
			if got.IsCredentialScoped() != want.IsCredentialScoped() || (got.RetryAfter() == nil) != (want.RetryAfter() == nil) ||
				(got.RetryAfter() != nil && *got.RetryAfter() != *want.RetryAfter()) {
				t.Fatalf("explicit 429 handling changed: got scoped=%v retry=%v, want scoped=%v retry=%v",
					got.IsCredentialScoped(), got.RetryAfter(), want.IsCredentialScoped(), want.RetryAfter())
			}
			if upstream.summaries.Load() != 0 {
				t.Fatal("explicit 429 queried the quota summary")
			}
		})
	}
}
