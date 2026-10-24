package helps

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const requestCompressionThreshold = 32 << 10

// CodexWebsocketWriteCompression permits outbound deflate only on verified first-party endpoints.
// Other upstreams retain their existing flate-tail compatibility precaution.
func CodexWebsocketWriteCompression(wsURL string) bool {
	endpoint, err := url.Parse(wsURL)
	if err != nil || endpoint.Scheme != "wss" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawPath != "" || endpoint.Fragment != "" {
		return false
	}
	if !strings.EqualFold(endpoint.Host, "chatgpt.com") && !strings.EqualFold(endpoint.Host, "chatgpt.com:443") {
		return false
	}
	return endpoint.Path == "/backend-api/codex/responses" || endpoint.Path == "/backend-api/codex/v1/responses"
}

var requestGzipPool = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return writer
}}

var requestZstdPool = sync.Pool{New: func() any {
	encoder, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	return encoder
}}

// WithRequestCompression wraps the transport before response usage tracking.
// body must contain the exact immutable finalized JSON used to construct the request.
// decodeError optionally reuses the provider's existing response decoder for 415 inspection.
func WithRequestCompression(client *http.Client, req *http.Request, body []byte, decodeError ...func(io.ReadCloser, string) (io.ReadCloser, error)) *http.Client {
	if requestCompressionEncoding(req, body) == "" {
		return client
	}
	wrapped := *client
	base := wrapped.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := requestCompressionRoundTripper{base: base, body: body}
	if len(decodeError) > 0 {
		transport.decodeError = decodeError[0]
	}
	wrapped.Transport = transport
	return &wrapped
}

type requestCompressionRoundTripper struct {
	base        http.RoundTripper
	body        []byte
	decodeError func(io.ReadCloser, string) (io.ReadCloser, error)
}

func (t requestCompressionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body := t.body
	if err := req.Context().Err(); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	compressed, encoding := compressUpstreamRequest(req, body)
	if encoding == "" {
		return t.base.RoundTrip(req)
	}
	// The client owns the cloned wire body, so release the bypassed original here.
	if req.Body != nil {
		defer req.Body.Close()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	wire := req.Clone(req.Context())
	setRequestBody(wire, compressed)
	wire.Header.Set("Content-Encoding", encoding)
	resp, err := t.base.RoundTrip(wire)
	if err != nil || resp.StatusCode != http.StatusUnsupportedMediaType || req.Context().Err() != nil {
		return resp, err
	}
	if !rejectsRequestEncoding(resp, encoding, t.decodeError) {
		return resp, nil
	}
	if err := req.Context().Err(); err != nil {
		return resp, nil
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	plain := req.Clone(req.Context())
	setRequestBody(plain, body)
	plain.Header.Del("Content-Encoding")
	// Do not repeat compression or retry any other response or transport error.
	return t.base.RoundTrip(plain)
}

func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	req.TransferEncoding = nil
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
}

func requestCompressionEncoding(req *http.Request, body []byte) string {
	if len(body) < requestCompressionThreshold || req == nil || req.URL == nil || req.Method != http.MethodPost || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Opaque != "" || req.URL.RawPath != "" || (req.URL.Port() != "" && req.URL.Port() != "443") || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) || req.Header.Get("Content-Encoding") != "" {
		return ""
	}
	encoding := ""
	switch strings.ToLower(req.URL.Hostname()) {
	case "api.anthropic.com":
		if req.URL.Path == "/v1/messages" {
			encoding = "gzip"
		}
	case "chatgpt.com":
		if req.URL.Path == "/backend-api/codex/responses" || req.URL.Path == "/backend-api/codex/v1/responses" {
			encoding = "zstd"
		}
	}
	mediaType, _, _ := strings.Cut(req.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), "application/json") {
		return ""
	}
	return encoding
}

func compressUpstreamRequest(req *http.Request, body []byte) ([]byte, string) {
	encoding := requestCompressionEncoding(req, body)
	if encoding == "" {
		return nil, ""
	}
	var compressed []byte
	if encoding == "gzip" {
		var output bytes.Buffer
		writer := requestGzipPool.Get().(*gzip.Writer)
		writer.Reset(&output)
		_, err := writer.Write(body)
		closeErr := writer.Close()
		// Detach the request-sized output before retaining the fixed encoder workspace.
		writer.Reset(io.Discard)
		requestGzipPool.Put(writer)
		if err != nil || closeErr != nil {
			return nil, ""
		}
		compressed = output.Bytes()
	} else {
		encoder := requestZstdPool.Get().(*zstd.Encoder)
		// Avoid EncodeAll's plaintext-sized default allocation for compressible histories.
		compressed = encoder.EncodeAll(body, make([]byte, 0, 4096))
		// EncodeAll owns no output writer and does not retain its returned slice.
		requestZstdPool.Put(encoder)
	}
	saving := len(body) - len(compressed)
	if saving < 1024 || saving < (len(body)+19)/20 {
		return nil, ""
	}
	return compressed, encoding
}

// rejectedBody preserves a bounded inspected prefix and the original close owner.
type rejectedBody struct {
	prefix   *bytes.Reader
	readErr  error
	original io.ReadCloser
}

func (b *rejectedBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if b.readErr != nil {
		err := b.readErr
		b.readErr = nil
		return 0, err
	}
	return b.original.Read(p)
}
func (b *rejectedBody) Close() error { return b.original.Close() }

func rejectsRequestEncoding(resp *http.Response, encoding string, decodeError func(io.ReadCloser, string) (io.ReadCloser, error)) bool {
	// RFC 7694 permits Accept-Encoding on a 415 response to identify supported codings.
	accept := strings.ToLower(strings.Join(resp.Header.Values("Accept-Encoding"), ","))
	identity, selected := false, false
	explicitSelected, wildcard := false, false
	for _, entry := range strings.Split(accept, ",") {
		coding, params, _ := strings.Cut(entry, ";")
		coding = strings.TrimSpace(coding)
		allowed := true
		for _, param := range strings.Split(params, ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.TrimSpace(name) == "q" {
				weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				allowed = err == nil && weight > 0 && weight <= 1
			}
		}
		identity = identity || (coding == "identity" && allowed)
		if coding == encoding {
			explicitSelected = true
			selected = selected || allowed
		} else if coding == "*" {
			wildcard = wildcard || allowed
		}
	}
	if !explicitSelected {
		selected = wildcard
	}
	if identity && !selected {
		return true
	}
	if resp.Body == nil {
		return false
	}
	const limit = 8 << 10
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	resp.Body = &rejectedBody{prefix: bytes.NewReader(prefix), readErr: err, original: resp.Body}
	if err != nil || len(prefix) > limit {
		return false
	}
	if decodeError != nil {
		decoded, decodeErr := decodeError(io.NopCloser(bytes.NewReader(prefix)), strings.Join(resp.Header.Values("Content-Encoding"), ","))
		if decodeErr != nil {
			return false
		}
		prefix, err = io.ReadAll(io.LimitReader(decoded, limit+1))
		_ = decoded.Close()
		if err != nil || len(prefix) > limit {
			return false
		}
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(prefix, &failure) == nil && failure.Error.Code == "unsupported_content_encoding"
}
