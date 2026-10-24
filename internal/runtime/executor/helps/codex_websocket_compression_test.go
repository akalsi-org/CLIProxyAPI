package helps

import "testing"

func TestCodexWebsocketWriteCompressionPolicy(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"wss://chatgpt.com/backend-api/codex/responses", true},
		{"wss://chatgpt.com:443/backend-api/codex/responses", true},
		{"wss://CHATGPT.COM/backend-api/codex/v1/responses", true},
		{"wss://chatgpt.com/backend-api/codex/v1/responses?session=fixture", true},
		{"ws://chatgpt.com/backend-api/codex/responses", false},
		{"https://chatgpt.com/backend-api/codex/responses", false},
		{"http://chatgpt.com/backend-api/codex/responses", false},
		{"wss://chatgpt.com:80/backend-api/codex/responses", false},
		{"wss://chatgpt.com:8443/backend-api/codex/responses", false},
		{"wss://chatgpt.com:0443/backend-api/codex/responses", false},
		{"wss://chatgpt.com:/backend-api/codex/responses", false},
		{"wss://chatgpt.com.evil.test/backend-api/codex/responses", false},
		{"wss://api.chatgpt.com/backend-api/codex/responses", false},
		{"wss://chatgpt.com./backend-api/codex/responses", false},
		{"wss://gateway.example/backend-api/codex/responses", false},
		{"wss://api.openai.com/backend-api/codex/responses", false},
		{"wss://user@chatgpt.com/backend-api/codex/responses", false},
		{"wss://user:password@chatgpt.com/backend-api/codex/responses", false},
		{"wss://chatgpt.com/backend-api/codex/responses/", false},
		{"wss://chatgpt.com/backend-api/codex/responses/compact", false},
		{"wss://chatgpt.com/v1/responses", false},
		{"wss://chatgpt.com/backend-api/codex/../codex/responses", false},
		{"wss://chatgpt.com/backend-api/codex/%72esponses", false},
		{"wss://chatgpt.com/backend-api/codex/responses#fragment", false},
		{"wss:chatgpt.com/backend-api/codex/responses", false},
		{"wss://chatgpt.com", false},
		{"", false},
		{"not a URL", false},
		{"wss://chatgpt.com:invalid/backend-api/codex/responses", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := CodexWebsocketWriteCompression(tc.url); got != tc.want {
				t.Fatalf("policy=%v want=%v", got, tc.want)
			}
		})
	}
}
