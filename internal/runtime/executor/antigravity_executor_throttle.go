package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// A bare RESOURCE_EXHAUSTED names no limit and gives no retry guidance. Google
// returns it for short-term throttling while account quota remains, so the
// account quota summary separates real exhaustion from throttling.
const (
	// The conductor floors supplied retry delays at ten seconds.
	antigravityThrottleBaseCooldown = 10 * time.Second
	antigravityThrottleMaxCooldown  = time.Minute
	antigravityQuotaSummaryCacheTTL = 30 * time.Second
	antigravityQuotaSummaryPath     = "/v1internal:retrieveUserQuotaSummary"
)

var antigravityThrottle = helps.NewAntigravityThrottleBackoff(antigravityThrottleBaseCooldown, antigravityThrottleMaxCooldown)

type antigravityQuotaSummaryEntry struct {
	fetchedAt time.Time
	body      []byte
}

// antigravityQuotaSummaryByAuth caches one quota summary per credential so
// concurrent throttling responses do not each query Google.
var antigravityQuotaSummaryByAuth sync.Map

func antigravityThrottleKey(auth *cliproxyauth.Auth, model string) string {
	if auth == nil {
		return "\x00" + model
	}
	return auth.ID + "\x00" + model
}

// resetAntigravityThrottle ends the backoff ladder after an accepted request.
func resetAntigravityThrottle(auth *cliproxyauth.Auth, model string) {
	antigravityThrottle.Reset(antigravityThrottleKey(auth, model))
}

// isAntigravityBareResourceExhausted reports a 429 RESOURCE_EXHAUSTED with no
// retry guidance and no quota or credits reason.
func isAntigravityBareResourceExhausted(statusCode int, body []byte) bool {
	if statusCode != http.StatusTooManyRequests || antigravityHasExplicitCreditsBalanceExhaustedReason(body) {
		return false
	}
	decision := decideAntigravity429(body)
	return decision.kind == antigravity429DecisionFullQuotaExhausted && decision.reason == "resource_exhausted"
}

// antigravityStatusErr builds the error for an upstream failure. A bare
// RESOURCE_EXHAUSTED cools only the failed model: until the quota reset when
// the account quota summary shows the model's group exhausted, and otherwise
// for a short exponential backoff.
func (e *AntigravityExecutor) antigravityStatusErr(ctx context.Context, auth *cliproxyauth.Auth, token, model string, statusCode int, body []byte) statusErr {
	err := newAntigravityStatusErr(e.cfg, statusCode, body)
	if !isAntigravityBareResourceExhausted(statusCode, body) || antigravityCoolingDisabled(auth, e.cfg) {
		return err
	}
	err.credentialScoped = false
	if until, exhausted := e.antigravityQuotaExhaustedUntil(ctx, auth, token, model); exhausted {
		delay := max(time.Until(until), antigravityThrottleBaseCooldown)
		err.retryAfter = &delay
		log.Infof("antigravity executor: quota exhausted for model %s; cooling until %s", model, until.UTC().Format(time.RFC3339))
		return err
	}
	delay := antigravityThrottle.Next(antigravityThrottleKey(auth, model))
	err.retryAfter = &delay
	log.Infof("antigravity executor: throttled without retry guidance for model %s; backing off %s", model, delay.Round(time.Millisecond))
	return err
}

// antigravityQuotaExhaustedUntil reports when the exhausted quota for model's
// group resets. A failed lookup reports no exhaustion.
func (e *AntigravityExecutor) antigravityQuotaExhaustedUntil(ctx context.Context, auth *cliproxyauth.Auth, token, model string) (time.Time, bool) {
	if auth == nil || strings.TrimSpace(token) == "" {
		return time.Time{}, false
	}
	now := time.Now()
	if cached, ok := antigravityQuotaSummaryByAuth.Load(auth.ID); ok {
		entry := cached.(antigravityQuotaSummaryEntry)
		if now.Sub(entry.fetchedAt) < antigravityQuotaSummaryCacheTTL {
			return antigravityQuotaGroupExhaustedUntil(entry.body, model, now)
		}
	}
	body, errFetch := e.fetchAntigravityQuotaSummary(ctx, auth, token)
	if errFetch != nil {
		log.Debugf("antigravity executor: quota summary unavailable: %v", errFetch)
		return time.Time{}, false
	}
	antigravityQuotaSummaryByAuth.Store(auth.ID, antigravityQuotaSummaryEntry{fetchedAt: now, body: body})
	return antigravityQuotaGroupExhaustedUntil(body, model, now)
}

func (e *AntigravityExecutor) fetchAntigravityQuotaSummary(ctx context.Context, auth *cliproxyauth.Auth, token string) ([]byte, error) {
	projectID, _ := e.projectIDForRequest(ctx, auth, token)
	reqBody, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		return nil, errMarshal
	}
	endpointURL := strings.TrimSuffix(resolveAntigravityRequestBaseURL(auth), "/") + antigravityQuotaSummaryPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(reqBody))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := helps.WithAntigravityHTTPClientTrace(httpClient, auth, "quota_summary").Do(httpReq)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close quota summary response body error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if errRead != nil {
		return nil, errRead
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, statusErr{code: httpResp.StatusCode, msg: "quota summary request failed"}
	}
	return body, nil
}

// antigravityQuotaGroupExhaustedUntil returns the latest future reset among
// exhausted buckets in the quota group that contains model. Proto3 JSON omits
// a zero remainingFraction, so a missing value means an exhausted bucket.
func antigravityQuotaGroupExhaustedUntil(body []byte, model string, now time.Time) (time.Time, bool) {
	family := "Gemini"
	switch lower := strings.ToLower(model); {
	case strings.HasPrefix(lower, "claude"):
		family = "Claude"
	case strings.HasPrefix(lower, "gpt"):
		family = "GPT"
	}
	var until time.Time
	for _, group := range gjson.GetBytes(body, "groups").Array() {
		if !strings.Contains(group.Get("displayName").String()+" "+group.Get("description").String(), family) {
			continue
		}
		for _, bucket := range group.Get("buckets").Array() {
			if bucket.Get("remainingFraction").Float() > 0 {
				continue
			}
			reset, errParse := time.Parse(time.RFC3339, bucket.Get("resetTime").String())
			if errParse != nil || !reset.After(now) {
				continue
			}
			if reset.After(until) {
				until = reset
			}
		}
	}
	return until, !until.IsZero()
}
