package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/client/grokbuild"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexHTTPWebsocketMessageLimit = 50 * 1024 * 1024

type codexHTTPWebsocketContextKey struct{}
type codexHTTPWebsocketBridge struct {
	key                     helps.CodexHTTPWebsocketKey
	pool                    *helps.CodexHTTPWebsocketPool
	lease                   *helps.CodexHTTPWebsocketLease
	resource                *codexHTTPWebsocketResource
	sent, clean, definitive bool
	stopCancel              func() bool
	cancelRequest           context.CancelFunc
}

func codexHTTPBridge(ctx context.Context) *codexHTTPWebsocketBridge {
	bridge, _ := ctx.Value(codexHTTPWebsocketContextKey{}).(*codexHTTPWebsocketBridge)
	return bridge
}

func (e *CodexAutoExecutor) httpWebsocketContext(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) (context.Context, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	enabled := e.httpExec.cfg != nil && e.httpExec.cfg.Codex.HTTPWebsockets
	e.httpPoolMu.Lock()
	if !enabled || e.httpPoolShutdown {
		pool := e.httpPool
		e.httpPool = nil
		e.httpPoolMu.Unlock()
		if pool != nil {
			pool.Close()
		}
		return ctx, false
	}
	if e.httpPool == nil {
		e.httpPool = helps.NewCodexHTTPWebsocketPool()
	}
	pool := e.httpPool
	e.httpPoolMu.Unlock()
	if cliproxyexecutor.DownstreamWebsocket(ctx) || !codexWebsocketsEnabled(auth) || opts.ExecutionLifecycle != nil || cliproxyexecutor.WebsocketInputFromContext(ctx) != nil || opts.Alt == "responses/compact" || isCodexOpenAIImageRequest(opts) || grokbuild.IsGrokClientContext(ctx, opts.Headers) || executionSessionIDFromOptions(opts) != "" || e.httpExec.cfg.PassthroughHeaders || cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return ctx, false
	}
	bridge := &codexHTTPWebsocketBridge{pool: pool}
	return context.WithValue(ctx, codexHTTPWebsocketContextKey{}, bridge), true
}

// Inspect the immutable body once. Absent sjson deletions otherwise allocate a full discarded body copy.
func codexHTTPPayload(ctx context.Context, body []byte) []byte {
	if codexHTTPBridge(ctx) == nil {
		return body
	}
	fields := [...]string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options"}
	root := util.ParseGJSONBytesNoCopy(body)
	var dropMask uint8
	streamSeen, streamTrue := false, false
	if root.IsObject() {
		root.ForEach(func(key, value gjson.Result) bool {
			switch key.String() {
			case "previous_response_id":
				dropMask |= 1
			case "generate":
				dropMask |= 2
			case "prompt_cache_retention":
				dropMask |= 4
			case "safety_identifier":
				dropMask |= 8
			case "stream_options":
				dropMask |= 16
			case "stream":
				if !streamSeen {
					streamSeen, streamTrue = true, value.Type == gjson.True
				}
			}
			return true
		})
	} else {
		// Retain existing mutation behavior for unusual SDK bodies.
		dropMask = 31
	}
	if !streamTrue {
		body = helps.SetBoolIfDifferent(body, "stream", true)
	}
	for index, key := range fields {
		if dropMask&(1<<index) != 0 {
			body, _ = sjson.DeleteBytes(body, key)
		}
	}
	return body
}

func codexWebsocketPayloadSelector(ctx context.Context) string {
	if codexHTTPBridge(ctx) != nil {
		return "codex"
	}
	return "codex-websockets"
}

// Derive provider headers through the HTTP path before introducing any private pool identity.
func (e *CodexWebsocketsExecutor) httpBridgeHeaders(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, from sdktranslator.Format, httpURL, baseModel, token string, body []byte) ([]byte, http.Header, error) {
	httpReq, body, err := e.cacheHelper(ctx, from, httpURL, req, body, opts.Headers)
	if err != nil {
		return nil, nil, err
	}
	applyCodexHeaders(httpReq, auth, token, true, e.cfg, opts.Headers)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, body, opts.Headers)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	headers := httpReq.Header
	// These two headers belong to HTTP framing, not provider identity.
	deleteHeaderCaseInsensitive(headers, "Connection")
	deleteHeaderCaseInsensitive(headers, "Content-Length")
	beta := headers.Get("OpenAI-Beta")
	if !strings.Contains(beta, "responses_websockets=") {
		if beta != "" {
			beta += ", "
		}
		headers.Set("OpenAI-Beta", beta+codexResponsesWebsocketBetaHeaderValue)
	}
	// Codex websocket v2 spells the existing HTTP session identity with an underscore.
	ensureCodexWebsocketSessionHeader(headers, opts.Headers, "")
	return body, headers, nil
}

func codexHTTPFingerprint(values ...any) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (b *codexHTTPWebsocketBridge) acquire(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, wsURL, proxyURL string, headers http.Header) (*codexWebsocketSession, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	caller := opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey]
	if caller == nil {
		caller = req.Metadata[cliproxyexecutor.CallerScopeMetadataKey]
	}
	if caller == nil {
		caller = helps.APIKeyFromContext(ctx)
	}
	credential := codexHTTPFingerprint(auth.ID)
	key := helps.CodexHTTPWebsocketKey{
		Credential:    credential,
		Epoch:         codexHTTPFingerprint(headerValueCaseInsensitive(headers, "Authorization")),
		Partition:     codexHTTPFingerprint(caller, auth.ID, wsURL, proxyURL),
		Compatibility: codexHTTPFingerprint(headers),
	}
	b.key = key
	lease, ok := b.pool.Acquire(ctx, key)
	if !ok {
		return nil, false, ctx.Err()
	}
	b.lease = lease
	if value := lease.Resource(); value != nil {
		b.resource = value.(*codexHTTPWebsocketResource)
	} else {
		resource := newCodexHTTPWebsocketResource()
		if !lease.SetResource(resource) {
			lease.Release(false)
			b.lease = nil
			return nil, false, ctx.Err()
		}
		b.resource = resource
	}
	return b.resource.session, true, nil
}

func (b *codexHTTPWebsocketBridge) requestContext(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	b.cancelRequest = cancel
	b.resource.mu.Lock()
	b.resource.requestCancel = cancel
	b.resource.currentResponseID = ""
	closed := b.resource.closed
	b.resource.mu.Unlock()
	if closed {
		cancel()
	}
	return ctx
}

func (b *codexHTTPWebsocketBridge) armCancellation(ctx context.Context) {
	b.stopCancel = context.AfterFunc(ctx, func() { _ = b.resource.Close() })
}

// Only this request owns cancellation. Join its callback before a healthy lease can change owners.
func (b *codexHTTPWebsocketBridge) finish(err error) error {
	if b == nil {
		return err
	}
	if b.lease == nil {
		if b.cancelRequest != nil {
			b.cancelRequest()
		}
		return err
	}
	stopped := true
	if b.stopCancel != nil {
		stopped = b.stopCancel()
	}
	healthy := b.clean && err == nil && stopped && b.resource.session.upstreamDisconnectError(b.resource.conn) == nil
	b.resource.mu.Lock()
	b.resource.requestCancel = nil
	b.resource.mu.Unlock()
	if b.cancelRequest != nil {
		b.cancelRequest()
	}
	b.lease.Release(healthy)
	return b.executionError(err)
}

func (b *codexHTTPWebsocketBridge) executionError(err error) error {
	if b == nil || !b.sent || err == nil || b.definitive {
		return err
	}
	return cliproxyexecutor.MarkExecutionUncertain(err)
}

type codexHTTPWebsocketResource struct {
	session                           *codexWebsocketSession
	mu                                sync.Mutex
	closed                            bool
	conn                              *websocket.Conn
	cancel                            context.CancelFunc
	requestCancel                     context.CancelFunc
	currentResponseID, lastResponseID string
	dialDone, readerDone              chan struct{}
}

func newCodexHTTPWebsocketResource() *codexHTTPWebsocketResource {
	r := &codexHTTPWebsocketResource{session: newEphemeralCodexWebsocketSession()}
	r.session.httpBridge = r
	return r
}

func (r *codexHTTPWebsocketResource) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.requestCancel != nil {
		r.requestCancel()
	}
	if r.cancel != nil {
		r.cancel()
	}
	conn, dialDone, readerDone := r.conn, r.dialDone, r.readerDone
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	// Cancel any blocked event delivery before joining the sole reader.
	r.session.activeMu.Lock()
	if r.session.activeCancel != nil {
		r.session.activeCancel()
	}
	r.session.activeMu.Unlock()
	if dialDone != nil {
		<-dialDone
	}
	if readerDone != nil {
		<-readerDone
	}
	return nil
}

func (e *CodexWebsocketsExecutor) ensureHTTPBridgeConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	r := sess.httpBridge
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("codex HTTP websocket: resource closed")
	}
	if r.conn != nil {
		conn := r.conn
		r.mu.Unlock()
		if err := sess.upstreamDisconnectError(conn); err != nil {
			return nil, nil, nil, &codexHTTPIdleSocketError{cause: err}
		}
		return conn, newWebsocketConnectionCloser(conn), nil, nil
	}
	dialCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.dialDone = make(chan struct{})
	dialDone := r.dialDone
	r.mu.Unlock()
	defer close(dialDone)
	conn, closer, response, err := e.dialCodexWebsocket(dialCtx, auth, wsURL, headers)
	if err != nil {
		return conn, closer, response, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = closer.Close()
		return nil, nil, response, context.Canceled
	}
	r.conn = conn
	r.readerDone = make(chan struct{})
	sess.connMu.Lock()
	sess.conn, sess.connCloser, sess.readerConn = conn, closer, conn
	sess.authID, sess.wsURL, sess.proxyURL = authID, wsURL, executionProxyURL(ctx, e.cfg, auth)
	sess.connMu.Unlock()
	sess.configureConn(conn)
	conn.SetReadLimit(codexHTTPWebsocketMessageLimit)
	go r.readLoop(conn)
	r.mu.Unlock()
	return conn, closer, response, nil
}

// NextReader inflates lazily. LimitReader bounds decoded bytes before ReadAll can materialize an oversized event.
func (r *codexHTTPWebsocketResource) readLoop(conn *websocket.Conn) {
	defer close(r.readerDone)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		kind, reader, err := conn.NextReader()
		var payload []byte
		if err == nil {
			payload, err = io.ReadAll(io.LimitReader(reader, codexHTTPWebsocketMessageLimit+1))
			if err == nil && len(payload) > codexHTTPWebsocketMessageLimit {
				err = &websocket.CloseError{Code: websocket.CloseMessageTooBig}
			}
			if err == nil && (kind != websocket.TextMessage || (len(bytes.TrimSpace(payload)) > 0 && (!gjson.ValidBytes(payload) || gjson.GetBytes(payload, "type").String() == ""))) {
				err = fmt.Errorf("codex HTTP websocket: malformed response event")
			}
		}
		if err == nil {
			err = r.validateResponse(payload)
		}
		if err != nil {
			r.session.setUpstreamDisconnectError(conn, err)
			_ = conn.Close()
		}
		ch, done := r.session.activeForConn(conn)
		if ch == nil {
			if err != nil {
				return
			}
			if codexHTTPConnectionNeutralEvent(payload) {
				continue
			}
			// An unsolicited idle response cannot belong to the next independent request.
			r.session.setUpstreamDisconnectError(conn, fmt.Errorf("codex HTTP websocket: unsolicited response event"))
			_ = conn.Close()
			return
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: kind, payload: payload, err: err}:
		case <-done:
		}
		if err != nil {
			return
		}
	}
}

var errCodexHTTPWebsocketCapacity = errors.New("codex HTTP websocket: local capacity unavailable")

type codexHTTPIdleSocketError struct{ cause error }

func (e *codexHTTPIdleSocketError) Error() string { return e.cause.Error() }
func (e *codexHTTPIdleSocketError) Unwrap() error { return e.cause }

// Replace a known-dead idle socket only before response.create is attempted.
// The old reader stops and its capacity returns before a fresh reservation can succeed.
func (e *CodexWebsocketsExecutor) ensureHTTPBridgeReady(ctx context.Context, auth *cliproxyauth.Auth, b *codexHTTPWebsocketBridge, sess *codexWebsocketSession, authID, wsURL string, headers http.Header) (*codexWebsocketSession, *websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	conn, closer, response, err := e.ensureHTTPBridgeConn(ctx, auth, sess, authID, wsURL, headers)
	var idleErr *codexHTTPIdleSocketError
	if !errors.As(err, &idleErr) {
		return sess, conn, closer, response, err
	}
	// Ownership moves before cancellation is armed, so retiring the old resource must not cancel this request.
	b.resource.mu.Lock()
	b.resource.requestCancel = nil
	b.resource.mu.Unlock()
	_ = b.resource.Close()
	b.lease.Release(false)
	b.lease, b.resource = nil, nil
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, nil, nil, nil, errCtx
	}
	lease, ok := b.pool.AcquireFresh(ctx, b.key)
	if !ok {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, nil, nil, nil, errCtx
		}
		return nil, nil, nil, nil, errCodexHTTPWebsocketCapacity
	}
	resource := newCodexHTTPWebsocketResource()
	if !lease.SetResource(resource) {
		lease.Release(false)
		return nil, nil, nil, nil, errCodexHTTPWebsocketCapacity
	}
	b.lease, b.resource = lease, resource
	resource.mu.Lock()
	resource.requestCancel = b.cancelRequest
	resource.mu.Unlock()
	sess = resource.session
	conn, closer, response, err = e.ensureHTTPBridgeConn(ctx, auth, sess, authID, wsURL, headers)
	return sess, conn, closer, response, err
}

// Only these connection-neutral events can arrive without a response identity or while the socket is idle.
func codexHTTPConnectionNeutralEvent(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 {
		return true
	}
	if gjson.GetBytes(payload, "response.id").Exists() || gjson.GetBytes(payload, "response_id").Exists() {
		return false
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "codex.rate_limits", "codex.response.metadata", "keepalive":
		return true
	default:
		return false
	}
}

// A completed response identity cannot become the first event of another independent request.
func (r *codexHTTPWebsocketResource) validateResponse(payload []byte) error {
	if codexHTTPConnectionNeutralEvent(payload) {
		return nil
	}
	eventType := gjson.GetBytes(payload, "type").String()
	terminal := eventType == "response.completed" || eventType == "response.done" || eventType == "response.incomplete"
	responseID := gjson.GetBytes(payload, "response.id").String()
	if eventType == "response.created" || terminal {
		if !gjson.GetBytes(payload, "response").IsObject() || gjson.GetBytes(payload, "response.id").Type != gjson.String || responseID == "" {
			return fmt.Errorf("codex HTTP websocket: malformed response envelope")
		}
	}
	if (eventType == "error" || eventType == "response.failed") && !codexHTTPDefinitiveFailure(payload) {
		return fmt.Errorf("codex HTTP websocket: malformed failure envelope")
	}
	if responseID == "" {
		responseID = gjson.GetBytes(payload, "response_id").String()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if responseID != "" {
		responseID = codexHTTPFingerprint(responseID)
		if responseID == r.lastResponseID || (r.currentResponseID != "" && responseID != r.currentResponseID) {
			return fmt.Errorf("codex HTTP websocket: stale response identity")
		}
		if r.currentResponseID == "" {
			r.currentResponseID = responseID
		}
	}
	if r.currentResponseID == "" && eventType != "error" && eventType != "response.failed" {
		return fmt.Errorf("codex HTTP websocket: response identity is missing")
	}
	if terminal {
		r.lastResponseID = responseID
	}
	return nil
}

// A synthesized native status must not turn a malformed HTTP bridge event into a replayable rejection.
func codexHTTPDefinitiveFailure(payload []byte) bool {
	eventType := gjson.GetBytes(payload, "type").String()
	var errorNode gjson.Result
	switch eventType {
	case "error":
		errorNode = gjson.GetBytes(payload, "body.error")
		if !errorNode.Exists() {
			errorNode = gjson.GetBytes(payload, "error")
		}
		for _, path := range []string{"status", "status_code"} {
			status := gjson.GetBytes(payload, path)
			if status.Exists() && (status.Type != gjson.Number || status.Int() < 400 || status.Int() > 599 || status.Float() != float64(status.Int())) {
				return false
			}
		}
	case "response.failed":
		response := gjson.GetBytes(payload, "response")
		id := response.Get("id")
		if !response.IsObject() || id.Type != gjson.String || strings.TrimSpace(id.String()) == "" {
			return false
		}
		if status := response.Get("status"); status.Exists() && (status.Type != gjson.String || status.String() != "failed") {
			return false
		}
		errorNode = response.Get("error")
		if !errorNode.Exists() {
			errorNode = gjson.GetBytes(payload, "error")
		}
	default:
		return false
	}
	if !errorNode.IsObject() {
		return false
	}
	for _, field := range []string{"message", "type", "code"} {
		value := errorNode.Get(field)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	return false
}

// Resolve environment proxy selection into the reuse key without exposing proxy credentials.
func codexHTTPBridgeProxyURL(cfgURL string, wsURL string) string {
	if cfgURL != "" {
		return cfgURL
	}
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	} else {
		parsed.Scheme = "http"
	}
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: parsed})
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.String()
}
