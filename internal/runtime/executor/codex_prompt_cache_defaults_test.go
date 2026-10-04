package executor

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketReadTimeoutDefaultOneHour(t *testing.T) {
	// These assertions protect requested defaults without waiting on wall-clock expiry.
	if codexResponsesWebsocketIdleTimeout != time.Hour {
		t.Fatalf("read timeout = %s, want one hour", codexResponsesWebsocketIdleTimeout)
	}
	if codexResponsesWebsocketHandshakeTO != 30*time.Second {
		t.Fatalf("handshake timeout changed: %s", codexResponsesWebsocketHandshakeTO)
	}
	if helps.CodexHTTPWebsocketIdleTTL != time.Minute {
		t.Fatalf("idle socket retention changed: %s", helps.CodexHTTPWebsocketIdleTTL)
	}
}

func TestCodexGPTPromptCacheHintDefaults(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-5.6*", Protocol: "codex"}, {Name: "gpt-6*", Protocol: "codex"}},
		Params: map[string]any{"prompt_cache_options.ttl": "30m"},
	}}}}
	for _, tc := range []struct {
		name     string
		model    string
		protocol string
		body     string
		want     string
	}{
		{"current GPT", "gpt-6.1-sol", "codex", `{"input":"hello"}`, "30m"},
		{"GPT 5.6", "gpt-5.6", "codex", `{"input":"hello"}`, "30m"},
		{"explicit caller setting", "gpt-6.1-sol", "codex", `{"prompt_cache_options":{"ttl":"caller-choice"}}`, "caller-choice"},
		{"older GPT", "gpt-5.5", "codex", `{"input":"hello"}`, ""},
		{"other provider", "grok-4.7", "xai", `{"input":"hello"}`, ""},
		{"other protocol", "gpt-6.1-sol", "openai", `{"input":"hello"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			out := helps.ApplyPayloadConfigWithRequestForExecutor(cfg, "codex", tc.model, tc.protocol,
				"openai-response", "", body, body, tc.model, "/v1/responses", nil)
			if got := gjson.GetBytes(out, "prompt_cache_options.ttl").String(); got != tc.want {
				t.Fatalf("cache hint = %q, want %q", got, tc.want)
			}
			// HTTP bridge normalization must not discard the new supported request option.
			if tc.want != "" {
				if got := gjson.GetBytes(codexHTTPPayload(t.Context(), out), "prompt_cache_options.ttl").String(); got != tc.want {
					t.Fatalf("HTTP normalization discarded cache hint: got %q", got)
				}
			}
		})
	}
}
