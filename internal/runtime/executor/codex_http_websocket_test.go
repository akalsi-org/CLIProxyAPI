package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexHTTPTestUpstream struct {
	server                   *httptest.Server
	upgrades, posts, creates atomic.Int32
	mu                       sync.Mutex
	headers                  []http.Header
	bodies                   [][]byte
	onCreate                 func(*websocket.Conn, []byte, string)
	handshakeStatus          int
}

func codexHTTPTestEvents(id string) [][]byte {
	return [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"object":"response","model":"gpt-5.4","status":"in_progress","output":[],"usage":null}}`, id)),
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg-test","type":"message","role":"assistant","status":"in_progress","content":[]}}`),
		[]byte(`{"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg-test","part":{"type":"output_text","text":"","annotations":[]}}`),
		[]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg-test","delta":"ok"}`),
		[]byte(`{"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"msg-test","text":"ok"}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg-test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}}`),
		[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":"gpt-5.4","status":"completed","output":[{"id":"msg-test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`, id)),
	}
}

func newCodexHTTPTestUpstream(t *testing.T, onCreate func(*websocket.Conn, []byte, string), status int) *codexHTTPTestUpstream {
	t.Helper()
	upstream := &codexHTTPTestUpstream{onCreate: onCreate, handshakeStatus: status}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: true}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			upstream.posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range codexHTTPTestEvents("resp-http") {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			return
		}
		if upstream.handshakeStatus != 0 {
			w.WriteHeader(upstream.handshakeStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
			return
		}
		// Count before the handshake response so callers cannot observe the upgrade first.
		upstream.upgrades.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		upstream.mu.Lock()
		upstream.headers = append(upstream.headers, r.Header.Clone())
		upstream.mu.Unlock()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			create := upstream.creates.Add(1)
			upstream.mu.Lock()
			upstream.bodies = append(upstream.bodies, bytes.Clone(payload))
			upstream.mu.Unlock()
			id := fmt.Sprintf("resp-%d", create)
			if upstream.onCreate != nil {
				upstream.onCreate(conn, payload, id)
			} else {
				for _, event := range codexHTTPTestEvents(id) {
					if conn.WriteMessage(websocket.TextMessage, event) != nil {
						return
					}
				}
			}
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func codexHTTPTestAuth(t *testing.T, upstream *codexHTTPTestUpstream) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test-token", "base_url": upstream.server.URL, "websockets": "true"}}
	auth.EnsureIndex()
	return auth
}
func codexHTTPTestExecutor(t *testing.T) *CodexAutoExecutor {
	cfg := &config.Config{}
	cfg.Codex.HTTPWebsockets = true
	exec := NewCodexAutoExecutor(cfg)
	t.Cleanup(func() { exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
	return exec
}
func codexHTTPTestRequest(format sdktranslator.Format) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	payload := `{"model":"gpt-5.4","input":"hello","previous_response_id":"must-not-send","generate":false}`
	if format == sdktranslator.FormatOpenAI {
		payload = `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`
	}
	if format == sdktranslator.FormatClaude {
		payload = `{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	}
	return cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(payload)}, cliproxyexecutor.Options{SourceFormat: format, Headers: http.Header{}, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller-a"}}
}
func codexHTTPTestContext() context.Context {
	gin.SetMode(gin.TestMode)
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return context.WithValue(context.Background(), "gin", gc)
}
func codexHTTPCollect(t *testing.T, stream *cliproxyexecutor.StreamResult) []byte {
	t.Helper()
	var out []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		out = append(out, chunk.Payload...)
	}
	return out
}

func TestCodexHTTPWebsocketFormatsReuseIndependentRequests(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, streaming), func(t *testing.T) {
				upstream := newCodexHTTPTestUpstream(t, nil, 0)
				auth := codexHTTPTestAuth(t, upstream)
				exec := codexHTTPTestExecutor(t)
				req, opts := codexHTTPTestRequest(format)
				opts.Stream = streaming
				ctx := codexHTTPTestContext()
				for range 2 {
					var out []byte
					if streaming {
						result, err := exec.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if result.Headers.Get("X-Request-Id") != "" {
							t.Fatal("handshake request identity leaked downstream")
						}
						out = codexHTTPCollect(t, result)
					} else {
						result, err := exec.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if !gjson.ValidBytes(result.Payload) {
							t.Fatalf("invalid response: %s", result.Payload)
						}
						out = result.Payload
					}
					if !bytes.Contains(out, []byte("ok")) {
						t.Fatalf("response missing text: %s", out)
					}
					if streaming && format != sdktranslator.FormatOpenAI && !bytes.Contains(out, []byte("data:")) {
						t.Fatalf("stream is not SSE: %s", out)
					}
					if cliproxyexecutor.DownstreamWebsocket(ctx) {
						t.Fatal("HTTP context became downstream websocket")
					}
				}
				if upstream.upgrades.Load() != 1 || upstream.posts.Load() != 0 || upstream.creates.Load() != 2 {
					t.Fatalf("upgrades=%d posts=%d creates=%d", upstream.upgrades.Load(), upstream.posts.Load(), upstream.creates.Load())
				}
				upstream.mu.Lock()
				defer upstream.mu.Unlock()
				for _, body := range upstream.bodies {
					if gjson.GetBytes(body, "type").String() != "response.create" || gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "generate").Exists() {
						t.Fatalf("HTTP normalization changed: %s", body)
					}
				}
				if !bytes.Equal(upstream.bodies[0], upstream.bodies[1]) {
					t.Fatal("independent request acquired implicit continuation state")
				}
			})
		}
	}
}

func TestCodexHTTPWebsocketCallerCredentialAndHeaderIsolation(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, nil, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	run := func() {
		t.Helper()
		if _, err := exec.Execute(codexHTTPTestContext(), auth, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "caller-b"
	run()
	opts.Headers.Set("X-Client-Request-Id", "request-1")
	run()
	opts.Headers.Set("X-Client-Request-Id", "request-2")
	run()
	auth.Attributes["api_key"] = "rotated-token"
	run()
	auth.ID += "-other"
	run()
	if got := upstream.upgrades.Load(); got != 6 {
		t.Fatalf("upgrades=%d, want six compatibility partitions", got)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.headers[2].Get("X-Client-Request-Id") != "request-1" || upstream.headers[3].Get("X-Client-Request-Id") != "request-2" || upstream.headers[4].Get("Authorization") != "Bearer rotated-token" {
		t.Fatal("caller header or credential was not preserved")
	}
}

func TestCodexHTTPWebsocketCapacityFallsBackBeforeCreate(t *testing.T) {
	entered := make(chan struct{}, helps.CodexHTTPWebsocketCredentialSlots)
	release := make(chan struct{})
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
		entered <- struct{}{}
		<-release
		for _, event := range codexHTTPTestEvents(id) {
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	var workers sync.WaitGroup
	errs := make(chan error, helps.CodexHTTPWebsocketCredentialSlots)
	for range helps.CodexHTTPWebsocketCredentialSlots {
		workers.Add(1)
		go func(selectedAuth *cliproxyauth.Auth) {
			defer workers.Done()
			_, err := exec.Execute(context.Background(), selectedAuth, req, opts)
			errs <- err
		}(auth.Clone())
	}
	for range helps.CodexHTTPWebsocketCredentialSlots {
		<-entered
	}
	result, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil || !bytes.Contains(result.Payload, []byte("ok")) {
		t.Fatalf("capacity fallback: %v %s", err, result.Payload)
	}
	if upstream.posts.Load() != 1 || upstream.creates.Load() != helps.CodexHTTPWebsocketCredentialSlots {
		t.Fatal("capacity fallback sent a websocket create")
	}
	close(release)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexHTTPWebsocketHandshakeFallbackAndRejection(t *testing.T) {
	for _, status := range []int{http.StatusUpgradeRequired, http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, nil, status)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			_, err := exec.Execute(context.Background(), auth, req, opts)
			if status == http.StatusUpgradeRequired {
				if err != nil || upstream.posts.Load() != 1 {
					t.Fatalf("426 fallback failed: %v", err)
				}
			} else {
				if err == nil || upstream.posts.Load() != 0 || cliproxyexecutor.IsExecutionUncertain(err) {
					t.Fatalf("definitive handshake rejection changed: %v", err)
				}
			}
		})
	}
}

func TestCodexHTTPWebsocketCancellationStopsReaderAndLateEvents(t *testing.T) {
	entered := make(chan struct{})
	allowLate := make(chan struct{})
	var first atomic.Bool
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
		if !first.Swap(true) {
			close(entered)
			<-allowLate
		}
		for _, event := range codexHTTPTestEvents(id) {
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := exec.Execute(ctx, auth, req, opts); result <- err }()
	<-entered
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) || !cliproxyexecutor.IsExecutionUncertain(err) {
		t.Fatalf("canceled generation was replayable: %v", err)
	}
	close(allowLate)
	response, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil || !bytes.Contains(response.Payload, []byte("ok")) || upstream.upgrades.Load() != 2 {
		t.Fatalf("late events crossed leases: %v", err)
	}
}

func TestCodexHTTPWebsocketMalformedAndDisconnectAreUncertain(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, _ string) {
				if malformed {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":`))
				}
				_ = conn.Close()
			}, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			_, err := exec.Execute(context.Background(), auth, req, opts)
			if err == nil || !cliproxyexecutor.IsExecutionUncertain(err) {
				t.Fatalf("post-send failure was replayable: %v", err)
			}
			if upstream.creates.Load() != 1 || upstream.posts.Load() != 0 {
				t.Fatal("post-send failure replayed generation")
			}
		})
	}
}

func TestCodexHTTPWebsocketStaleCancellationAndShutdown(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, nil, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	if upstream.upgrades.Load() != 1 {
		t.Fatal("stale cancellation closed a healthy next lease")
	}
	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	if upstream.posts.Load() != 1 {
		t.Fatal("shutdown generation resurrected")
	}
}

func TestCodexHTTPWebsocketDisabledAndPassthroughRemainHTTP(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, nil, 0)
			exec := codexHTTPTestExecutor(t)
			exec.httpExec.cfg.Codex.HTTPWebsockets = passthrough
			exec.httpExec.cfg.PassthroughHeaders = passthrough
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			result, err := exec.Execute(context.Background(), auth, req, opts)
			if err != nil || !strings.Contains(string(result.Payload), "ok") {
				t.Fatal(err)
			}
			if upstream.posts.Load() != 1 || upstream.upgrades.Load() != 0 {
				t.Fatal("legacy HTTP eligibility changed")
			}
		})
	}
}

func TestCodexHTTPWebsocketPartialWritesNeverReplay(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, nil, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			req.Payload = []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", 3*codexWebsocketWriteChunkSize) + `"}`)
			var current *websocket.Conn
			testWebsocketWritePayloadHook = func(conn *websocket.Conn) { current = conn }
			testWebsocketWriteChunkHook = func(index, _ int) {
				if index == 1 {
					_ = current.Close()
				}
			}
			defer func() { testWebsocketWritePayloadHook = nil; testWebsocketWriteChunkHook = nil }()
			var err error
			if streaming {
				_, err = exec.ExecuteStream(context.Background(), auth, req, opts)
			} else {
				_, err = exec.Execute(context.Background(), auth, req, opts)
			}
			if err == nil || !cliproxyexecutor.IsExecutionUncertain(err) {
				t.Fatalf("partial write remained replayable: %v", err)
			}
			if upstream.upgrades.Load() != 1 || upstream.posts.Load() != 0 {
				t.Fatal("partial write retried or fell back to HTTP")
			}
		})
	}
}

func TestCodexHTTPWebsocketStreamDisconnectBeforeAndAfterOutput(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprint(output), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
				if output {
					for _, event := range codexHTTPTestEvents(id)[:4] {
						if conn.WriteMessage(websocket.TextMessage, event) != nil {
							return
						}
					}
				}
				_ = conn.Close()
			}, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var terminal error
			var text []byte
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					terminal = chunk.Err
				}
				text = append(text, chunk.Payload...)
			}
			if terminal == nil || !cliproxyexecutor.IsExecutionUncertain(terminal) {
				t.Fatalf("stream disconnect was replayable: %v", terminal)
			}
			if output && !bytes.Contains(text, []byte("ok")) {
				t.Fatal("output disappeared before stream disconnect")
			}
			if upstream.creates.Load() != 1 || upstream.posts.Load() != 0 {
				t.Fatal("stream disconnect replayed generation")
			}
		})
	}
}

func TestCodexHTTPWebsocketDecodedMessageLimit(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, _ string) {
		conn.EnableWriteCompression(true)
		payload := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", codexHTTPWebsocketMessageLimit) + `"}`)
		_ = conn.WriteMessage(websocket.TextMessage, payload)
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	_, err := exec.Execute(context.Background(), auth, req, opts)
	var tooBig codexWebsocketMessageTooBigError
	if !errors.As(err, &tooBig) || tooBig.StatusCode() != http.StatusRequestEntityTooLarge || !cliproxyexecutor.IsExecutionUncertain(err) {
		t.Fatalf("decoded limit did not reject oversized compressed message: %v", err)
	}
	if upstream.creates.Load() != 1 || upstream.posts.Load() != 0 {
		t.Fatal("oversized event replayed generation")
	}
}

func TestCodexHTTPWebsocketEventBacklogIsOne(t *testing.T) {
	resource := newCodexHTTPWebsocketResource()
	conn := &websocket.Conn{}
	events := resource.session.activate(conn)
	defer resource.session.clearActive(conn, events)
	if cap(events) != 1 {
		t.Fatalf("event backlog=%d, want one", cap(events))
	}
}

func TestCodexHTTPWebsocketActiveShutdownStopsBlockedProducer(t *testing.T) {
	entered := make(chan struct{})
	allow := make(chan struct{})
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
		close(entered)
		for _, event := range codexHTTPTestEvents(id)[:4] {
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
		<-allow
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	// Shutdown cancels the request-local producer even when nobody consumes its unbuffered output.
	for range result.Chunks {
	}
	close(allow)
}

func TestCodexHTTPWebsocketDisableAndReplacementRetireGeneration(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, nil, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	exec.httpExec.cfg.Codex.HTTPWebsockets = false
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	exec.httpExec.cfg.Codex.HTTPWebsockets = true
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
	replacement := codexHTTPTestExecutor(t)
	if _, err := replacement.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	if upstream.upgrades.Load() != 3 || upstream.posts.Load() != 1 {
		t.Fatalf("generation retirement: upgrades=%d posts=%d", upstream.upgrades.Load(), upstream.posts.Load())
	}
}

func TestCodexHTTPWebsocketProviderQuotaRejectionIsDefinitive(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, _ string) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"quota exceeded","code":"rate_limit_exceeded"}}`))
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil || cliproxyexecutor.IsExecutionUncertain(err) {
		t.Fatalf("definitive quota rejection changed: %v", err)
	}
	if upstream.posts.Load() != 0 {
		t.Fatal("quota rejection bypassed account handling through HTTP")
	}
}

func TestCodexHTTPWebsocketPayloadRulesAndProviderIdentity(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, nil, 0)
	exec := codexHTTPTestExecutor(t)
	exec.httpExec.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-*", Protocol: "codex"}}, Params: map[string]any{"service_tier": "priority", "previous_response_id": "rule-ancestry", "generate": false}}}
	auth := codexHTTPTestAuth(t, upstream)
	auth.Attributes["header:X-Test-Session"] = "$CPA-SESSION-ID"
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = "caller-conversation"
	opts.Headers.Set("Session-Id", "caller-conversation")
	for range 2 {
		if _, err := exec.Execute(codexHTTPTestContext(), auth, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.headers[0].Get("X-Test-Session") != cliproxyauth.CanonicalSessionID(opts.Headers, req.Payload, opts.Metadata) {
		t.Fatal("private pool identity replaced provider session identity")
	}
	for _, body := range upstream.bodies {
		if gjson.GetBytes(body, "service_tier").String() != "priority" || gjson.GetBytes(body, "previous_response_id").Exists() || gjson.GetBytes(body, "generate").Exists() {
			t.Fatalf("HTTP payload rules changed: %s", body)
		}
	}
	if upstream.upgrades.Load() != 1 {
		t.Fatal("stable provider session did not reuse its private socket")
	}
}

func TestCodexHTTPWebsocketMainAndImageUsageParity(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
				events := codexHTTPTestEvents(id)
				events[len(events)-1], _ = sjson.SetRawBytes(events[len(events)-1], "response.tool_usage", []byte(`{"image_gen":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`))
				for _, event := range events {
					if conn.WriteMessage(websocket.TextMessage, event) != nil {
						return
					}
				}
			}, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			capture := &codexResponseModelUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })
			ctx := coreusage.WithRequestedModelAlias(context.Background(), t.Name())
			if streaming {
				result, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				codexHTTPCollect(t, result)
			} else {
				if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			main := capture.await(t)
			image := capture.await(t)
			if main.Failed || main.Detail.InputTokens != 3 || main.Detail.OutputTokens != 2 || main.Detail.TotalTokens != 5 {
				t.Fatalf("main usage changed: %+v", main)
			}
			if image.Failed || image.Model != codexDefaultImageToolModel || image.Detail.TotalTokens != 30 {
				t.Fatalf("image usage changed: %+v", image)
			}
		})
	}
}

func TestCodexHTTPWebsocketApplyPatchNativeToolParity(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"output":[]}}`, id)))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"bad-patch","type":"function_call","call_id":"bad-call","name":"apply_patch","arguments":"{}"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[{"type":"function_call","id":"bad-patch","call_id":"bad-call","name":"apply_patch","arguments":"{}"}]}}`, id)))
			}, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAI)
			req.Payload = []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"edit"}],"tools":[{"type":"custom","name":"apply_patch"}]}`)
			var err error
			var out []byte
			if streaming {
				result, errStart := exec.ExecuteStream(context.Background(), auth, req, opts)
				if errStart != nil {
					t.Fatal(errStart)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						err = chunk.Err
					}
					out = append(out, chunk.Payload...)
				}
			} else {
				result, errExecute := exec.Execute(context.Background(), auth, req, opts)
				err = errExecute
				out = result.Payload
			}
			// Native Codex keeps raw tool input, matching the existing HTTP translator contract.
			if err != nil || !bytes.Contains(out, []byte("apply_patch")) {
				t.Fatalf("native apply-patch tool parity: %v %s", err, out)
			}
		})
	}
}

func TestCodexHTTPWebsocketBootstrapBufferingRetainsReuse(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, nil, 0)
	exec := codexHTTPTestExecutor(t)
	exec.httpExec.cfg.Codex.StreamBootstrapBuffering = true
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	for range 2 {
		result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(codexHTTPCollect(t, result), []byte("ok")) {
			t.Fatal("bootstrap lost output")
		}
	}
	if upstream.upgrades.Load() != 1 {
		t.Fatal("bootstrap buffering prevented healthy reuse")
	}
}

func TestCodexHTTPWebsocketBootstrapImmediateTerminalRetainsReuse(t *testing.T) {
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
		events := codexHTTPTestEvents(id)
		_ = conn.WriteMessage(websocket.TextMessage, events[len(events)-1])
	}, 0)
	exec := codexHTTPTestExecutor(t)
	exec.httpExec.cfg.Codex.StreamBootstrapBuffering = true
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	for range 2 {
		result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		codexHTTPCollect(t, result)
	}
	if upstream.upgrades.Load() != 1 {
		t.Fatal("immediate terminal response did not return a healthy lease")
	}
}

func TestCodexHTTPWebsocketMalformedFailureCannotUseConfiguredContinue(t *testing.T) {
	cases := []struct{ name, payload string }{
		{"bare-error", `{"type":"error"}`},
		{"bare-failed", `{"type":"response.failed"}`},
		{"status-only", `{"type":"error","status":429}`},
		{"empty-error", `{"type":"error","status":502,"error":{}}`},
		{"null-error", `{"type":"error","status":502,"error":null}`},
		{"failed-without-identity", `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"rejected"}}}`},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{false, true} {
			for _, bootstrap := range []bool{false, true} {
				for _, action := range []string{"continue", "continue-and-cooldown"} {
					t.Run(fmt.Sprintf("%s/stream=%t/bootstrap=%t/%s", tc.name, streaming, bootstrap, action), func(t *testing.T) {
						upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, _ string) {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(tc.payload))
						}, 0)
						exec := codexHTTPTestExecutor(t)
						cfg := exec.httpExec.cfg
						cfg.Codex.StreamBootstrapBuffering = bootstrap
						cfg.OAuthRequestScopedErrors = map[string][]config.RequestScopedErrorRule{"codex": {
							{Status: 502, Action: action},
							{Match: []string{"malformed"}, Action: action},
						}}
						manager := cliproxyauth.NewManager(nil, nil, nil)
						manager.SetConfig(cfg)
						manager.SetRetryConfig(5, 0, 6)
						manager.RegisterExecutor(exec)
						reg := registry.GetGlobalRegistry()
						for index := range 2 {
							auth := codexHTTPTestAuth(t, upstream)
							auth.ID = fmt.Sprintf("%s-credential-%d", t.Name(), index)
							auth.Status = cliproxyauth.StatusActive
							delete(auth.Attributes, "api_key")
							auth.Metadata = map[string]any{"access_token": "test-token"}
							reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
							t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
							if _, err := manager.Register(context.Background(), auth); err != nil {
								t.Fatal(err)
							}
						}
						req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
						var err error
						if streaming {
							result, errStart := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
							err = errStart
							if result != nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						} else {
							_, err = manager.Execute(context.Background(), []string{"codex"}, req, opts)
						}
						if err == nil || !cliproxyexecutor.IsExecutionUncertain(err) {
							t.Fatalf("malformed provider failure remained replayable: %v", err)
						}
						if upstream.creates.Load() != 1 || upstream.posts.Load() != 0 {
							t.Fatalf("malformed failure replayed generation: creates=%d posts=%d", upstream.creates.Load(), upstream.posts.Load())
						}
					})
				}
			}
		}
	}
}

func TestCodexHTTPWebsocketStructuredFailureRemainsDefinitive(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","status":401,"error":{"type":"authentication_error","message":"rejected"}}`,
		`{"type":"error","status":429,"body":{"error":{"code":"rate_limit_exceeded","message":"rejected"}}}`,
		`{"type":"response.failed","response":{"id":"failed-response","status":"failed","error":{"type":"rate_limit_error","message":"rejected"}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, _ string) {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
			}, 0)
			exec := codexHTTPTestExecutor(t)
			auth := codexHTTPTestAuth(t, upstream)
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			_, err := exec.Execute(context.Background(), auth, req, opts)
			if err == nil || cliproxyexecutor.IsExecutionUncertain(err) {
				t.Fatalf("structured rejection lost its policy: %v", err)
			}
		})
	}
}

func TestCodexHTTPWebsocketDeadIdleSocketRedialsWithoutConductorRetry(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			closeIdle := make(chan struct{})
			var first atomic.Bool
			upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
				for _, event := range codexHTTPTestEvents(id) {
					if conn.WriteMessage(websocket.TextMessage, event) != nil {
						return
					}
				}
				if !first.Swap(true) {
					<-closeIdle
					_ = conn.Close()
				}
			}, 0)
			exec := codexHTTPTestExecutor(t)
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetConfig(exec.httpExec.cfg)
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(exec)
			auth := codexHTTPTestAuth(t, upstream)
			auth.Status = cliproxyauth.StatusActive
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
			observed := make(chan *codexHTTPWebsocketResource, 1)
			var observeOnce sync.Once
			opts.WebSocketResponseObserver = func(ctx context.Context, _ cliproxyexecutor.WebSocketResponseEvent) {
				observeOnce.Do(func() { observed <- codexHTTPBridge(ctx).resource })
			}
			run := func() {
				t.Helper()
				var out []byte
				if streaming {
					result, err := manager.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					out = codexHTTPCollect(t, result)
				} else {
					result, err := manager.Execute(context.Background(), []string{"codex"}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					out = result.Payload
				}
				if !bytes.Contains(out, []byte("ok")) {
					t.Fatalf("missing response after idle retirement: %s", out)
				}
			}
			run()
			resource := <-observed
			close(closeIdle)
			<-resource.readerDone
			run()
			if upstream.upgrades.Load() != 2 || upstream.creates.Load() != 2 || upstream.posts.Load() != 0 {
				t.Fatalf("idle replacement changed request count: upgrades=%d creates=%d posts=%d", upstream.upgrades.Load(), upstream.creates.Load(), upstream.posts.Load())
			}
		})
	}
}

func TestCodexHTTPWebsocketConnectionNeutralEventsBeforeResponseAndWhileIdle(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, bootstrap := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/bootstrap=%t", streaming, bootstrap), func(t *testing.T) {
				neutral := [][]byte{
					[]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42}}}`),
					[]byte(`{"type":"codex.response.metadata","metadata":{"transport":"websocket"}}`),
					[]byte(`{"type":"keepalive"}`),
					[]byte(" \n"),
				}
				idleWrite := make(chan struct{})
				idleProcessed := make(chan struct{}, 1)
				var first atomic.Bool
				upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, _ []byte, id string) {
					conn.SetPongHandler(func(string) error { idleProcessed <- struct{}{}; return nil })
					for _, event := range neutral {
						if conn.WriteMessage(websocket.TextMessage, event) != nil {
							return
						}
					}
					for _, event := range codexHTTPTestEvents(id) {
						if conn.WriteMessage(websocket.TextMessage, event) != nil {
							return
						}
					}
					if !first.Swap(true) {
						<-idleWrite
						for _, event := range neutral {
							if conn.WriteMessage(websocket.TextMessage, event) != nil {
								return
							}
						}
						_ = conn.WriteMessage(websocket.PingMessage, []byte("idle-metadata-read"))
					}
				}, 0)
				exec := codexHTTPTestExecutor(t)
				exec.httpExec.cfg.Codex.StreamBootstrapBuffering = bootstrap
				auth := codexHTTPTestAuth(t, upstream)
				req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
				var observed atomic.Int32
				opts.WebSocketResponseObserver = func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
					switch event.EventType {
					case "codex.rate_limits", "codex.response.metadata", "keepalive":
						observed.Add(1)
					}
				}
				run := func() {
					t.Helper()
					if streaming {
						result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						codexHTTPCollect(t, result)
					} else {
						if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
							t.Fatal(err)
						}
					}
				}
				run()
				close(idleWrite)
				<-idleProcessed
				run()
				if upstream.upgrades.Load() != 1 || upstream.creates.Load() != 2 || observed.Load() != 6 {
					t.Fatalf("neutral event compatibility changed: upgrades=%d creates=%d observed=%d", upstream.upgrades.Load(), upstream.creates.Load(), observed.Load())
				}
			})
		}
	}
}

func TestCodexHTTPWebsocketNormalizationMatchesExistingMutations(t *testing.T) {
	ctx := context.WithValue(context.Background(), codexHTTPWebsocketContextKey{}, &codexHTTPWebsocketBridge{})
	cases := []string{
		`{"model":"gpt-5.4","stream":true,"input":"hello"}`,
		`{"stream":false,"input":"hello","previous_response_id":"old","generate":false,"prompt_cache_retention":"24h","safety_identifier":"identifier","stream_options":{"include_usage":true}}`,
		`{"previous_response_id":null,"generate":null,"prompt_cache_retention":null,"safety_identifier":null,"stream_options":null}`,
		`{"stream":"true","nested":{"previous_response_id":"retain","generate":false},"input":[{"role":"user","content":"hello"}]}`,
		`{"stream":false,"stream":true,"generate":1,"generate":2}`,
		`{"previous_response_id":"old","stream":true}`,
		`{}`,
		`[]`,
	}
	for _, payload := range cases {
		t.Run(payload, func(t *testing.T) {
			body := []byte(payload)
			original := bytes.Clone(body)
			want := helps.SetBoolIfDifferent(body, "stream", true)
			for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options"} {
				want, _ = sjson.DeleteBytes(want, field)
			}
			got := codexHTTPPayload(ctx, body)
			if !bytes.Equal(got, want) {
				t.Fatalf("normalization changed: got=%s want=%s", got, want)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("normalization changed caller-owned input")
			}
		})
	}
}

func TestCodexHTTPWebsocketNormalizationDoesNotCopyCanonicalLargeBody(t *testing.T) {
	ctx := context.WithValue(context.Background(), codexHTTPWebsocketContextKey{}, &codexHTTPWebsocketBridge{})
	body := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", 1<<20) + `","stream":true}`)
	var out []byte
	allocations := testing.AllocsPerRun(20, func() { out = codexHTTPPayload(ctx, body) })
	if allocations != 0 || len(out) != len(body) || &out[0] != &body[0] {
		t.Fatalf("canonical normalization allocated or copied: allocations=%g", allocations)
	}
}

func TestCodexHTTPWebsocketFrameUsesSanitizedHTTPBody(t *testing.T) {
	body := helps.SanitizeCodexInputItemIDs([]byte(`{"model":"gpt-5.4","input":[{"type":"message","id":"original","content":"hello"}]}`))
	original := bytes.Clone(body)
	got := frameCodexWebsocketRequestBody(body)
	want := buildCodexWebsocketRequestBody(body)
	if !bytes.Equal(got, want) || !bytes.Equal(body, original) {
		t.Fatalf("HTTP frame encoding changed: got=%s want=%s", got, want)
	}
	if gjson.GetBytes(got, "input.0.id").String() != "msg_original" {
		t.Fatal("sanitized HTTP item identity changed")
	}
}

func TestCodexHTTPWebsocketWriteBufferBudget(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	native := exec.codexWebsocketDialer(context.Background(), nil)
	baseline := newProxyAwareWebsocketDialer(context.Background(), exec.cfg, nil)
	ctx := context.WithValue(context.Background(), codexHTTPWebsocketContextKey{}, &codexHTTPWebsocketBridge{})
	bridge := exec.codexWebsocketDialer(ctx, nil)
	if native.WriteBufferSize != baseline.WriteBufferSize || native.WriteBufferSize != 0 {
		t.Fatal("native websocket write-buffer policy changed")
	}
	if bridge.WriteBufferSize != codexWebsocketWriteChunkSize {
		t.Fatalf("HTTP write buffer=%d, want one upload chunk", bridge.WriteBufferSize)
	}
	if bridge.WriteBufferSize != 32*1024 {
		t.Fatalf("HTTP write buffer=%d, want 32 KiB", bridge.WriteBufferSize)
	}
	// The pinned Gorilla version uses 4 KiB when WriteBufferSize is zero.
	incremental := (bridge.WriteBufferSize - 4*1024) * helps.CodexHTTPWebsocketGlobalSlots
	if incremental != 896*1024 {
		t.Fatalf("process-wide incremental write-buffer budget=%d, want 896 KiB", incremental)
	}
}

func TestCodexHTTPWebsocketBufferedFragmentedUploadIsByteExact(t *testing.T) {
	text := strings.Repeat("fragment-\\\"-µ\n", 16*1024)
	captured := make(chan []byte, 1)
	upstream := newCodexHTTPTestUpstream(t, func(conn *websocket.Conn, payload []byte, id string) {
		captured <- bytes.Clone(payload)
		for _, event := range codexHTTPTestEvents(id) {
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}, 0)
	exec := codexHTTPTestExecutor(t)
	auth := codexHTTPTestAuth(t, upstream)
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	var err error
	req.Payload, err = json.Marshal(map[string]any{"model": "gpt-5.4", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks, total int
	testWebsocketWriteChunkHook = func(index, count int) {
		if index != chunks {
			t.Errorf("upload chunk index=%d, want %d", index, chunks)
		}
		chunks++
		total = count
	}
	defer func() { testWebsocketWriteChunkHook = nil }()
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	payload := <-captured
	if !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "type").String() != "response.create" || gjson.GetBytes(payload, "input.0.content.0.text").String() != text {
		t.Fatal("fragmented upload changed JSON or text bytes")
	}
	expected := (len(payload) + codexWebsocketWriteChunkSize - 1) / codexWebsocketWriteChunkSize
	if chunks != expected || total != expected || chunks < 3 {
		t.Fatalf("upload chunk contract changed: chunks=%d total=%d expected=%d", chunks, total, expected)
	}
	if upstream.creates.Load() != 1 {
		t.Fatal("fragmented upload replayed its create")
	}
}

func TestCodexHTTPWebsocketPongDuringBufferedFragmentedUpload(t *testing.T) {
	entered := make(chan struct{})
	confirmed := make(chan struct{})
	serverPong := make(chan string, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetPongHandler(func(payload string) error { serverPong <- payload; return nil })
		readDone := make(chan error, 1)
		go func() { _, _, err := conn.ReadMessage(); readDone <- err }()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Error("upload did not reach its third chunk")
			return
		}
		if err := conn.WriteControl(websocket.PingMessage, []byte("http-buffered-upload"), time.Now().Add(time.Second)); err != nil {
			t.Error(err)
			return
		}
		select {
		case payload := <-serverPong:
			if payload != "http-buffered-upload" {
				t.Errorf("pong payload=%q", payload)
			}
			close(confirmed)
		case <-time.After(2 * time.Second):
			t.Error("pong was blocked by a buffered upload")
			return
		}
		if err := <-readDone; err != nil {
			t.Error(err)
			return
		}
		for _, event := range codexHTTPTestEvents("buffered-upload-response") {
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	exec := codexHTTPTestExecutor(t)
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL, "websockets": "true"}}
	req, opts := codexHTTPTestRequest(sdktranslator.FormatOpenAIResponse)
	req.Payload = []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", 6*codexWebsocketWriteChunkSize) + `"}`)
	testWebsocketWriteChunkHook = func(index, _ int) {
		if index != 2 {
			return
		}
		close(entered)
		select {
		case <-confirmed:
		case <-time.After(2 * time.Second):
			t.Error("pong did not finish while the upload request lock was held")
		}
	}
	defer func() { testWebsocketWriteChunkHook = nil }()
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
}
