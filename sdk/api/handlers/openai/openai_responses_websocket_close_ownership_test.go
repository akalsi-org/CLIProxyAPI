package openai

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// Hold the terminal frame before its network write, not an ordinary data write.
type terminalErrorWriteGate struct {
	net.Conn
	started   chan struct{}
	release   <-chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (c *terminalErrorWriteGate) Write(payload []byte) (int, error) {
	if bytes.Contains(payload, []byte(`"type":"error"`)) || (len(payload) > 0 && payload[0] == 0x80|websocket.CloseMessage) {
		c.once.Do(func() {
			close(c.started)
			select {
			case <-c.release:
			case <-c.closed:
			}
		})
	}
	return c.Conn.Write(payload)
}

func (c *terminalErrorWriteGate) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type terminalErrorGateResponseWriter struct {
	http.ResponseWriter
	started chan struct{}
	release <-chan struct{}
}

func (w terminalErrorGateResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &terminalErrorWriteGate{Conn: conn, started: w.started, release: w.release, closed: make(chan struct{})}, buffered, nil
}

func TestResponsesWebsocketTerminalCloseWaitsForErrorFrameOwner(t *testing.T) {
	serverResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := make(chan struct{})
		release := make(chan struct{})
		conn, err := responsesWebsocketUpgrader.Upgrade(terminalErrorGateResponseWriter{ResponseWriter: w, started: started, release: release}, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer func() { _ = conn.Close() }()
		writer := newResponsesWebsocketWriter(conn)
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: websocketPinnedFailoverStatusError{status: http.StatusTooManyRequests, msg: `{"error":{"type":"usage_limit_reached","message":"quota exhausted"}}`}}
		ownerDone := make(chan error, 1)
		go func() {
			_, _, errWrite := writeResponsesWebsocketTerminalError(writer, nil, errMsg, nil)
			ownerDone <- errWrite
		}()
		<-started
		otherEntered := make(chan struct{})
		otherDone := make(chan error, 1)
		go func() {
			close(otherEntered)
			_, _, errWrite := writeResponsesWebsocketTerminalError(writer, nil, errMsg, nil)
			// Model the forwarding handler's deferred socket teardown.
			_ = conn.Close()
			otherDone <- errWrite
		}()
		<-otherEntered
		select {
		case <-otherDone:
			close(release)
			<-ownerDone
			serverResult <- errors.New("competing terminal close returned before the owner delivered its error frame")
			return
		case <-time.After(50 * time.Millisecond):
			// The gate fixes the ownership ordering; this guard detects an early return.
		}
		close(release)
		for _, done := range []<-chan error{ownerDone, otherDone} {
			if errWrite := <-done; !errors.Is(errWrite, websocket.ErrCloseSent) {
				serverResult <- fmt.Errorf("terminal close: %w", errWrite)
				return
			}
		}
		serverResult <- nil
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, frame, errRead := conn.ReadMessage()
	if errServer := <-serverResult; errServer != nil {
		t.Fatal(errServer)
	}
	if errRead != nil || ojson.GetBytesNoCopy(frame, "type").String() != "error" || ojson.GetBytesNoCopy(frame, "status").Int() != http.StatusTooManyRequests {
		t.Fatalf("terminal frame = %s, err=%v; want the owner's 429 error before socket teardown", frame, errRead)
	}
}

// net.Pipe has no send buffer: without a peer reader, an application write blocks.
type terminalErrorPipeListener struct {
	ready chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *terminalErrorPipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.ready:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *terminalErrorPipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *terminalErrorPipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestResponsesWebsocketTerminalCloseUnblocksOnCancellationOrTransportLoss(t *testing.T) {
	for _, tc := range []struct{ owner, action string }{
		{"payload", "cancel"},
		{"payload", "transport-close"},
		{"control", "cancel"},
		{"control", "transport-close"},
	} {
		t.Run(tc.owner+"/"+tc.action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverPipe, clientPipe := net.Pipe()
			defer func() { _ = serverPipe.Close(); _ = clientPipe.Close() }()
			started := make(chan struct{})
			// A closed gate only observes the terminal write; net.Pipe itself blocks it.
			release := make(chan struct{})
			close(release)
			listener := &terminalErrorPipeListener{ready: make(chan net.Conn, 1), done: make(chan struct{})}
			listener.ready <- serverPipe
			serverResult := make(chan error, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(terminalErrorGateResponseWriter{ResponseWriter: w, started: started, release: release}, r, nil)
				if err != nil {
					serverResult <- err
					return
				}
				defer func() { _ = conn.Close() }()
				writer := newResponsesWebsocketWriter(conn, ctx)
				payload := []byte(`{"type":"error","status":429,"error":{"message":"quota exhausted"}}`)
				ownerDone := make(chan error, 1)
				otherDone := make(chan error, 1)
				go func() {
					if tc.owner == "control" {
						_, errWrite := writer.closeForUpstreamError(&websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "message too big"})
						ownerDone <- errWrite
						return
					}
					_, errWrite := writer.closeWithPayload(payload)
					ownerDone <- errWrite
				}()
				<-started
				go func() { _, errWrite := writer.closeWithPayload(payload); otherDone <- errWrite }()
				for index, done := range []<-chan error{ownerDone, otherDone} {
					select {
					case errWrite := <-done:
						if index == 0 && errWrite == nil {
							serverResult <- errors.New("terminal network write succeeded without a peer reader")
							return
						}
					case <-time.After(5 * time.Second):
						_ = conn.Close()
						serverResult <- errors.New("terminal close owner or joining caller leaked after teardown")
						return
					}
				}
				select {
				case <-writer.closeDone:
					serverResult <- nil
				default:
					serverResult <- errors.New("terminal close returned without completing ownership")
				}
			}))
			_ = server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			dialer := websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) { return clientPipe, nil }}
			conn, _, err := dialer.Dial("ws://127.0.0.1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// Do not read application frames. The terminal owner's network write cannot complete.
			<-started
			if tc.action == "cancel" {
				cancel()
			} else {
				_ = conn.Close()
			}
			if errServer := <-serverResult; errServer != nil {
				t.Fatal(errServer)
			}
		})
	}
}
