package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type compressionCaptureExecutor struct {
	payload []byte
	source  string
	mode    string
}

func (*compressionCaptureExecutor) Identifier() string { return "compression-test" }
func (e *compressionCaptureExecutor) capture(req coreexecutor.Request, opts coreexecutor.Options, mode string) {
	e.payload = bytes.Clone(req.Payload)
	e.source = opts.SourceFormat.String()
	e.mode = mode
}
func (e *compressionCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture(req, opts, "execute")
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *compressionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.capture(req, opts, "stream")
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (*compressionCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (e *compressionCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture(req, opts, "count")
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":7}`)}, nil
}
func (*compressionCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func compressIngressBody(t *testing.T, plain []byte, encoding string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "zstd":
		var err error
		writer, err = zstd.NewWriter(&buffer)
		if err != nil {
			t.Fatal(err)
		}
	default:
		return plain
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestCompressedIngressHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct{ path, source, mode string }{
		{"/v1/messages", "claude", "execute"},
		{"/v1/messages?stream=true", "claude", "stream"},
		{"/v1/messages/count_tokens", "claude", "count"},
		{"/v1/chat/completions", "openai", "execute"},
		{"/v1/responses", "openai-response", "execute"},
		{"/v1/responses/compact", "openai-response", "execute"},
	} {
		for _, encoding := range []string{"identity", "gzip", "zstd"} {
			t.Run(endpoint.path+"/"+encoding, func(t *testing.T) {
				executor := &compressionCaptureExecutor{}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				auth := &coreauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "compression-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
				claudeHandler := NewClaudeCodeAPIHandler(base)
				openaiHandler := openai.NewOpenAIAPIHandler(base)
				responsesHandler := openai.NewOpenAIResponsesAPIHandler(base)
				router := gin.New()
				router.Use(func(c *gin.Context) {
					c.Set(handlers.MaxRequestBodyBytesContextKey, int64(256))
					c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256)
				})
				router.POST("/v1/messages", claudeHandler.ClaudeMessages)
				router.POST("/v1/messages/count_tokens", claudeHandler.ClaudeCountTokens)
				router.POST("/v1/chat/completions", openaiHandler.ChatCompletions)
				router.POST("/v1/responses", responsesHandler.Responses)
				router.POST("/v1/responses/compact", responsesHandler.Compact)
				plain := []byte(fmt.Sprintf(`{"model":"compression-model","stream":%t,"messages":[{"role":"user","content":"hello"}],"input":"hello"}`, endpoint.mode == "stream"))
				for _, variant := range []string{"valid", "stale", "decoded limit", "wire limit", "truncated", "corrupt"} {
					if (variant == "truncated" && encoding == "identity") || (variant == "corrupt" && encoding != "gzip") {
						continue
					}
					t.Run(variant, func(t *testing.T) {
						executor.payload = nil
						body := plain
						status := http.StatusOK
						switch variant {
						case "decoded limit":
							body = []byte(`{"input":"` + strings.Repeat("x", 512) + `"}`)
							status = http.StatusRequestEntityTooLarge
						case "wire limit":
							status = http.StatusRequestEntityTooLarge
						case "truncated", "corrupt":
							status = http.StatusBadRequest
						}
						if variant != "stale" {
							body = compressIngressBody(t, body, encoding)
						}
						if variant == "wire limit" {
							body = bytes.Repeat([]byte("x"), 257)
						}
						if variant == "truncated" {
							body = body[:len(body)-3]
						}
						if variant == "corrupt" {
							body[len(body)-8] ^= 0xff
						}
						req := httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(body))
						req.ContentLength = -1
						req.Header.Set("Content-Encoding", encoding)
						req.Header.Set("Content-Type", "application/json")
						recorder := httptest.NewRecorder()
						router.ServeHTTP(recorder, req)
						if recorder.Code != status {
							t.Fatalf("status=%d want=%d body=%s", recorder.Code, status, recorder.Body.String())
						}
						if status != http.StatusOK {
							if executor.payload != nil {
								t.Fatal("invalid body reached executor")
							}
							return
						}
						if executor.mode != endpoint.mode || executor.source != endpoint.source {
							t.Fatalf("mode=%s source=%s", executor.mode, executor.source)
						}
						if gjson.GetBytes(executor.payload, "messages.0.content").String() != "hello" {
							t.Fatalf("payload=%s", executor.payload)
						}
						if endpoint.mode == "count" && recorder.Body.String() != `{"input_tokens":7}` {
							t.Fatalf("count=%s", recorder.Body.String())
						}
						if endpoint.mode == "stream" && !strings.Contains(recorder.Body.String(), "message_stop") {
							t.Fatalf("stream=%s", recorder.Body.String())
						}
					})
				}
			})
		}
	}
}
