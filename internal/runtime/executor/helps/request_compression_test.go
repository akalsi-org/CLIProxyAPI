package helps

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type requestCompressionTransport func(*http.Request) (*http.Response, error)

func (f requestCompressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sendCompressionTestRequest(client *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	return WithRequestCompression(client, req, body).Do(req)
}

func compressionHistory() []byte {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	messages := make([]message, 96)
	for i := range messages {
		messages[i] = message{Role: "user", Content: fmt.Sprintf("Turn %d: inspect module_%d.go. Preserve JSON semantics and cancellation. %s", i, i%13, strings.Repeat("The deterministic fixture contains source review context, not user transcripts. ", 24))}
	}
	body, _ := json.Marshal(struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}{"fixture", messages})
	return body
}
func compressionRequest(t testing.TB, endpoint string, body []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	return r
}
func readCompressedRequest(t *testing.T, r *http.Request, expected []byte, coding string) {
	t.Helper()
	wire, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(wire)) != r.ContentLength {
		t.Fatal("incorrect ContentLength")
	}
	replay, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := io.ReadAll(replay)
	_ = replay.Close()
	if err != nil || !bytes.Equal(wire, replayed) {
		t.Fatal("GetBody did not replay wire bytes")
	}
	var decoded []byte
	switch coding {
	case "gzip":
		decoder, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err = io.ReadAll(decoder)
		_ = decoder.Close()
		if err != nil {
			t.Fatal(err)
		}
	case "zstd":
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err = decoder.DecodeAll(wire, nil)
		decoder.Close()
		if err != nil {
			t.Fatal(err)
		}
	default:
		decoded = wire
	}
	if !bytes.Equal(decoded, expected) {
		t.Fatal("wire body differs from finalized JSON")
	}
}

func TestRequestCompressionCodecsAndReplay(t *testing.T) {
	for _, tc := range []struct{ endpoint, coding string }{
		{"https://api.anthropic.com/v1/messages?beta=true", "gzip"},
		{"https://chatgpt.com/backend-api/codex/responses", "zstd"},
		{"https://chatgpt.com/backend-api/codex/v1/responses", "zstd"},
	} {
		t.Run(tc.coding+tc.endpoint, func(t *testing.T) {
			body := compressionHistory()
			req := compressionRequest(t, tc.endpoint, body)
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Content-Length", "999")
			responseBody := io.NopCloser(strings.NewReader("data: original\n\n"))
			client := &http.Client{Transport: requestCompressionTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Content-Encoding") != tc.coding {
					t.Fatal("wrong encoding")
				}
				if r.Header.Get("Content-Length") != "" {
					t.Fatal("stale Content-Length")
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Fatal("response negotiation changed")
				}
				readCompressedRequest(t, r, body, tc.coding)
				return &http.Response{StatusCode: 200, Body: responseBody, Header: http.Header{"Content-Encoding": {"gzip"}}}, nil
			})}
			resp, err := sendCompressionTestRequest(client, req, body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Body != responseBody || resp.Header.Get("Content-Encoding") != "gzip" {
				t.Fatal("response stream changed")
			}
			_ = resp.Body.Close()
			if req.Header.Get("Content-Encoding") != "" || req.ContentLength != int64(len(body)) {
				t.Fatal("original request mutated")
			}
		})
	}
}

func TestRequestCompressionEligibility(t *testing.T) {
	body := compressionHistory()
	for _, endpoint := range []string{
		"https://api.anthropic.com/v1/messages/count_tokens", "https://api.anthropic.com/v1/messages/",
		"https://api.anthropic.com:8443/v1/messages", "http://api.anthropic.com/v1/messages",
		"https://api.anthropic.com.evil.test/v1/messages", "https://user@api.anthropic.com/v1/messages",
		"https://custom.test/v1/messages", "https://api.openai.com/v1/responses",
		"https://chatgpt.com/backend-api/codex/responses/compact", "https://chatgpt.com/v1/responses",
	} {
		t.Run(endpoint, func(t *testing.T) {
			r := compressionRequest(t, endpoint, body)
			if _, coding := compressUpstreamRequest(r, body); coding != "" {
				t.Fatal("compressed unverified endpoint")
			}
		})
	}
	for _, modify := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") },
		func(r *http.Request) { r.Header.Set("Content-Type", "application/octet-stream") },
		func(r *http.Request) { r.Host = "custom.test" },
		func(r *http.Request) { r.Method = http.MethodGet },
	} {
		r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
		modify(r)
		if _, coding := compressUpstreamRequest(r, body); coding != "" {
			t.Fatal("compressed ineligible request")
		}
	}
}

func TestRequestCompressionSmallAndIncompressible(t *testing.T) {
	for _, host := range []string{"api.anthropic.com/v1/messages", "chatgpt.com/backend-api/codex/responses"} {
		for _, n := range []int{0, 1024, (32 << 10) - 1} {
			body := bytes.Repeat([]byte("a"), n)
			r := compressionRequest(t, "https://"+host, body)
			if allocs := testing.AllocsPerRun(100, func() {
				if _, coding := compressUpstreamRequest(r, body); coding != "" {
					panic("small request compressed")
				}
			}); allocs != 0 {
				t.Fatalf("small bypass allocations = %v", allocs)
			}
		}
		body := make([]byte, 256<<10)
		_, _ = rand.New(rand.NewSource(73)).Read(body)
		r := compressionRequest(t, "https://"+host, body)
		if _, coding := compressUpstreamRequest(r, body); coding != "" {
			t.Fatal("incompressible input should bypass")
		}
	}
	body := compressionHistory()
	r := compressionRequest(t, "https://custom.test/v1/messages", body)
	if allocs := testing.AllocsPerRun(100, func() { compressUpstreamRequest(r, body) }); allocs != 0 {
		t.Fatalf("custom bypass allocations = %v", allocs)
	}
}

func TestRequestCompressionFallback(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		response, accept string
		retry            bool
	}{
		{"explicit code", 415, `{"error":{"code":"unsupported_content_encoding"}}`, "", true},
		{"identity header", 415, "", "identity", true},
		{"forbidden identity", 415, "", "identity;q=0", false},
		{"invalid identity weight", 415, "", "identity;q=invalid", false},
		{"weighted identity", 415, "", "identity;q=0.5", true},
		{"unrelated 415", 415, `{"error":{"code":"unsupported_media_type"}}`, "", false},
		{"supported coding", 415, "", "gzip, identity", false},
		{"rejected coding", 415, "", "gzip;q=0, identity", true},
		{"explicit rejection overrides wildcard", 415, "", "gzip;q=0, *;q=1, identity", true},
		{"explicit acceptance overrides wildcard", 415, "", "gzip;q=1, *;q=0, identity", false},
		{"large error", 415, strings.Repeat("x", 9000), "", false},
		{"bad request", 400, `{"error":{"code":"unsupported_content_encoding"}}`, "", false},
		{"rate limited", 429, "", "identity", false},
		{"server error", 500, "", "identity", false},
		{"success", 200, "data: success\n\n", "identity", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := compressionHistory()
			r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
			calls := 0
			client := &http.Client{Transport: requestCompressionTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					readCompressedRequest(t, req, body, "gzip")
				} else {
					if calls > 2 || req.Header.Get("Content-Encoding") != "" {
						t.Fatal("fallback repeated compression")
					}
					readCompressedRequest(t, req, body, "")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Accept-Encoding": {tc.accept}}, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})}
			resp, err := sendCompressionTestRequest(client, r, body)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != tc.response {
				t.Fatal("rejected response body was changed")
			}
			want := 1
			if tc.retry {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}

func TestRequestCompressionZstdExplicitCodingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		accept string
		calls  int
	}{
		{"zstd;q=0, *;q=1, identity", 2},
		{"zstd;q=1, *;q=0, identity", 1},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			body := compressionHistory()
			r := compressionRequest(t, "https://chatgpt.com/backend-api/codex/responses", body)
			calls := 0
			client := &http.Client{Transport: requestCompressionTransport(func(wire *http.Request) (*http.Response, error) {
				calls++
				coding := "zstd"
				if calls > 1 {
					coding = ""
				}
				readCompressedRequest(t, wire, body, coding)
				return &http.Response{StatusCode: 415, Header: http.Header{"Accept-Encoding": {tc.accept}}, Body: http.NoBody}, nil
			})}
			resp, err := WithRequestCompression(client, r, body).Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if calls != tc.calls {
				t.Fatalf("calls=%d want=%d", calls, tc.calls)
			}
		})
	}
}

func TestRequestCompressionCancellationAndTransportFailure(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		body := compressionHistory()
		ctx, stop := context.WithCancel(context.Background())
		r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body).WithContext(ctx)
		calls := 0
		client := &http.Client{Transport: requestCompressionTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if cancel {
				stop()
				return &http.Response{StatusCode: 415, Header: http.Header{"Accept-Encoding": {"identity"}}, Body: io.NopCloser(strings.NewReader("rejected"))}, nil
			}
			return nil, errors.New("transport failure")
		})}
		resp, _ := sendCompressionTestRequest(client, r, body)
		if resp != nil {
			_ = resp.Body.Close()
		}
		stop()
		if calls != 1 {
			t.Fatalf("retried cancellation or transport failure: %d", calls)
		}
	}
}

func TestRequestCompressionConcurrentAndLocalHTTP(t *testing.T) {
	body := compressionHistory()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readCompressedRequestWire := r.Header.Get("Content-Encoding")
		if readCompressedRequestWire != "gzip" {
			t.Error("missing compression on local wire")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: local\n\n")
	}))
	defer server.Close()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
			client := &http.Client{Transport: requestCompressionTransport(func(wire *http.Request) (*http.Response, error) {
				local := wire.Clone(wire.Context())
				local.URL.Scheme = "http"
				local.URL.Host = strings.TrimPrefix(server.URL, "http://")
				local.Host = local.URL.Host
				return http.DefaultTransport.RoundTrip(local)
			})}
			resp, err := sendCompressionTestRequest(client, r, body)
			if err != nil {
				t.Error(err)
				return
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != "data: local\n\n" {
				t.Error("stream response changed")
			}
		}()
	}
	group.Wait()
}

type compressionTrackedBody struct {
	reader        io.Reader
	reads, closes int
}

func (b *compressionTrackedBody) Read(p []byte) (int, error) { b.reads++; return b.reader.Read(p) }
func (b *compressionTrackedBody) Close() error               { b.closes++; return nil }

type compressionErrorReader struct{ err error }

func (r compressionErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRequestCompressionBodyOwnershipAndReadError(t *testing.T) {
	body := compressionHistory()
	for _, canceled := range []bool{false, true} {
		r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
		original := &compressionTrackedBody{reader: bytes.NewReader(body)}
		r.Body = original
		ctx, stop := context.WithCancel(r.Context())
		defer stop()
		r = r.WithContext(ctx)
		if canceled {
			stop()
		}
		stream := &compressionTrackedBody{reader: strings.NewReader("data: unread\n\n")}
		calls := 0
		client := &http.Client{Transport: requestCompressionTransport(func(wire *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: stream, Header: make(http.Header)}, nil
		})}
		resp, err := sendCompressionTestRequest(client, r, body)
		if canceled && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
		if canceled && calls != 0 {
			t.Fatal("sent canceled request")
		}
		if original.closes != 1 || original.reads != 0 {
			t.Fatalf("original body reads=%d closes=%d", original.reads, original.closes)
		}
		if stream.reads != 0 {
			t.Fatal("read successful SSE before returning")
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	readErr := errors.New("bounded response read failure")
	original := &compressionTrackedBody{reader: io.MultiReader(strings.NewReader(`{"error":{"code":"unsupported_content_encoding"}}`), compressionErrorReader{readErr})}
	r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
	calls := 0
	client := &http.Client{Transport: requestCompressionTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 415, Body: original, Header: make(http.Header)}, nil
	})}
	resp, err := sendCompressionTestRequest(client, r, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, readErr) || string(got) != `{"error":{"code":"unsupported_content_encoding"}}` || calls != 1 || original.closes != 1 {
		t.Fatalf("response read evidence lost: body=%q err=%v calls=%d closes=%d", got, err, calls, original.closes)
	}
}

func TestRequestCompressionRejectedResponseDoesNotSetTTFT(t *testing.T) {
	body := compressionHistory()
	r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
	reporter := NewUsageReporter(context.Background(), "claude", "fixture", nil)
	calls := 0
	base := &http.Client{Transport: requestCompressionTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 415, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unsupported_content_encoding"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: success\n\n"))}, nil
	})}
	client := reporter.TrackHTTPClient(WithRequestCompression(base, r, body))
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reporter.IsTTFTSet() {
		t.Fatalf("rejection set successful TTFT: calls=%d ttft=%v", calls, reporter.IsTTFTSet())
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !reporter.IsTTFTSet() {
		t.Fatal("successful response did not set TTFT")
	}
}

func TestRequestCompressionEncodedInspectionBounds(t *testing.T) {
	body := compressionHistory()
	for _, tc := range []struct {
		name    string
		raw     []byte
		encoded bool
	}{
		{"raw limit", bytes.Repeat([]byte("x"), 9000), false},
		{"malformed", []byte("malformed gzip"), true},
		{"decoded limit", bytes.Repeat([]byte("x"), 9000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			if tc.name == "decoded limit" {
				var output bytes.Buffer
				gz := gzip.NewWriter(&output)
				_, _ = gz.Write(raw)
				_ = gz.Close()
				raw = output.Bytes()
			}
			original := &compressionTrackedBody{reader: bytes.NewReader(raw)}
			r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
			calls, decodes := 0, 0
			client := &http.Client{Transport: requestCompressionTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 415, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: original}, nil
			})}
			client = WithRequestCompression(client, r, body, func(input io.ReadCloser, _ string) (io.ReadCloser, error) { decodes++; return gzip.NewReader(input) })
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if calls != 1 || err != nil || !bytes.Equal(got, raw) || original.closes != 1 {
				t.Fatalf("rejected bytes/ownership changed: calls=%d err=%v closes=%d", calls, err, original.closes)
			}
			if tc.name == "raw limit" && decodes != 0 {
				t.Fatal("decoded oversized raw response")
			}
		})
	}
}

func TestRequestCompressionAttachmentBypass(t *testing.T) {
	body := compressionHistory()
	base := &http.Client{}
	for _, tc := range []struct {
		url  string
		body []byte
	}{
		{"https://api.anthropic.com/v1/messages", body[:1024]},
		{"https://custom.test/v1/messages", body},
	} {
		r := compressionRequest(t, tc.url, tc.body)
		if allocs := testing.AllocsPerRun(100, func() {
			if WithRequestCompression(base, r, tc.body) != base {
				panic("bypass cloned client")
			}
		}); allocs != 0 {
			t.Fatalf("attachment bypass allocations=%v", allocs)
		}
	}
}

func TestRequestCompressionEmptyRejectionBody(t *testing.T) {
	body := compressionHistory()
	r := compressionRequest(t, "https://api.anthropic.com/v1/messages", body)
	calls := 0
	client := &http.Client{Transport: requestCompressionTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 415, Header: http.Header{"Accept-Encoding": {"identity"}}}, nil
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	resp, err := WithRequestCompression(client, r, body).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Fatalf("empty rejection did not fallback: %d", calls)
	}
}

func BenchmarkRequestCompressionBoundary(b *testing.B) {
	history := compressionHistory()
	for _, tc := range []struct {
		name, url string
		body      []byte
	}{
		{"gzip_history", "https://api.anthropic.com/v1/messages", history},
		{"zstd_history", "https://chatgpt.com/backend-api/codex/responses", history},
		{"small_bypass", "https://api.anthropic.com/v1/messages", history[:1024]},
		{"custom_bypass", "https://custom.test/v1/messages", history},
	} {
		for _, send := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/send=%v", tc.name, send), func(b *testing.B) {
				r := compressionRequest(b, tc.url, tc.body)
				response := &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}
				base := &http.Client{Transport: requestCompressionTransport(func(r *http.Request) (*http.Response, error) { _ = r.Body.Close(); return response, nil })}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					client := WithRequestCompression(base, r, tc.body)
					if send {
						resp, err := client.Transport.RoundTrip(r)
						if err != nil {
							b.Fatal(err)
						}
						_ = resp.Body.Close()
					}
				}
			})
		}
	}
}

func BenchmarkRequestCompression(b *testing.B) {
	history := compressionHistory()
	// Seeded base64 simulates an already-compressed attachment inside valid JSON.
	random := make([]byte, 128<<10)
	_, _ = rand.New(rand.NewSource(19)).Read(random)
	attachment := []byte(`{"attachment":"` + base64.StdEncoding.EncodeToString(random) + `"}`)
	for _, tc := range []struct {
		name, url string
		body      []byte
	}{
		{"gzip_BestSpeed_history", "https://api.anthropic.com/v1/messages", history},
		{"zstd_SpeedFastest_history", "https://chatgpt.com/backend-api/codex/responses", history},
		{"gzip_BestSpeed_attachment", "https://api.anthropic.com/v1/messages", attachment},
		{"zstd_SpeedFastest_attachment", "https://chatgpt.com/backend-api/codex/responses", attachment},
		{"small_bypass", "https://api.anthropic.com/v1/messages", history[:1024]},
		{"custom_identity_bypass", "https://custom.test/v1/messages", history},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := compressionRequest(b, tc.url, tc.body)
			wire, coding := compressUpstreamRequest(r, tc.body)
			wireBytes := len(wire)
			if coding == "" {
				wireBytes = len(tc.body)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.body)))
			b.ResetTimer()
			for range b.N {
				compressUpstreamRequest(r, tc.body)
			}
			b.ReportMetric(float64(len(tc.body)), "plain-bytes")
			b.ReportMetric(float64(wireBytes), "wire-bytes")
		})
	}
}
