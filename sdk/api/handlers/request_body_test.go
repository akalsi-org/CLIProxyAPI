package handlers

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func encodeRequestBody(t testing.TB, body []byte, encoding string) []byte {
	t.Helper()
	for _, enc := range strings.Split(encoding, ",") {
		var out bytes.Buffer
		var writer io.WriteCloser
		switch strings.TrimSpace(enc) {
		case "", "identity":
			continue
		case "gzip":
			writer = gzip.NewWriter(&out)
		case "zstd":
			var err error
			writer, err = zstd.NewWriter(&out)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body = out.Bytes()
	}
	return body
}

func TestReadRequestBodyCompression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plain := []byte(`{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
	for _, encoding := range []string{"", "identity", "gzip", "zstd", "gzip, zstd", "zstd, gzip", "identity, gzip, identity"} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stale=%t", encoding, stale), func(t *testing.T) {
				body := plain
				if !stale {
					body = encodeRequestBody(t, plain, encoding)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
				c.Request.Header.Set("Content-Encoding", encoding)
				c.Set(MaxRequestBodyBytesContextKey, int64(max(len(plain), len(body))+64))
				got, err := ReadRequestBody(c)
				if err != nil || !bytes.Equal(got, plain) {
					t.Fatalf("body=%q error=%v", got, err)
				}
			})
		}
	}
}

func TestReadRequestBodyCompressionErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plain := []byte(`{"input":"` + strings.Repeat("x", 1024) + `"}`)
	valid := encodeRequestBody(t, plain, "gzip")
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 0xff
	for _, tc := range []struct {
		name, encoding string
		body           []byte
		limit          int64
		tooLarge       bool
	}{
		{"truncated", "gzip", valid[:len(valid)-3], 0, false},
		{"corrupt", "gzip", corrupt, 0, false},
		{"invalid", "gzip", []byte("not gzip"), 0, false},
		{"unsupported", "br", []byte("not json"), 0, false},
		{"gzip limit", "gzip", valid, 128, true},
		{"zstd limit", "zstd", encodeRequestBody(t, plain, "zstd"), 128, true},
		{"stack limit", "gzip, zstd", encodeRequestBody(t, plain, "gzip, zstd"), 128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tc.body))
			c.Request.Header.Set("Content-Encoding", tc.encoding)
			c.Set(MaxRequestBodyBytesContextKey, tc.limit)
			_, err := ReadRequestBody(c)
			var limitErr *http.MaxBytesError
			if err == nil || errors.As(err, &limitErr) != tc.tooLarge {
				t.Fatalf("error=%v, want tooLarge=%v", err, tc.tooLarge)
			}
		})
	}
}

func TestDecodeRequestBodyLimitBoundaries(t *testing.T) {
	plain := []byte(`{"input":"` + strings.Repeat("x", 1024) + `"}`)
	for _, encoding := range []string{"gzip", "zstd", "gzip, zstd", "zstd, gzip"} {
		body := encodeRequestBody(t, plain, encoding)
		for _, limit := range []int64{0, int64(len(plain) - 1), int64(len(plain)), int64(len(plain) + 1)} {
			t.Run(fmt.Sprintf("%s/limit=%d", encoding, limit), func(t *testing.T) {
				decoded, err := decodeRequestBody(body, encoding, limit)
				if limit == int64(len(plain)-1) {
					var limitErr *http.MaxBytesError
					if !errors.As(err, &limitErr) || limitErr.Limit != limit {
						t.Fatalf("error=%v, want limit=%d", err, limit)
					}
				} else if err != nil || !bytes.Equal(decoded, plain) {
					t.Fatalf("body=%q error=%v", decoded, err)
				}
			})
		}
	}
}

func TestDecodeGzipRequestBodyConcatenatedMembers(t *testing.T) {
	first := encodeRequestBody(t, []byte(`{"input":`), "gzip")
	second := encodeRequestBody(t, []byte(`"hello"}`), "gzip")
	decoded, err := decodeRequestBody(append(first, second...), "gzip", 17)
	if err != nil || string(decoded) != `{"input":"hello"}` {
		t.Fatalf("body=%q error=%v", decoded, err)
	}
}

func BenchmarkReadRequestBodyIdentity(b *testing.B) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"input":"` + strings.Repeat("x", 4096) + `"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	reader := bytes.NewReader(payload)
	c.Request.Body = io.NopCloser(reader)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		reader.Reset(payload)
		if _, err := ReadRequestBody(c); err != nil {
			b.Fatal(err)
		}
	}
}

func gzipBody(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, errWrite := w.Write(plain); errWrite != nil {
		t.Fatalf("gzip write: %v", errWrite)
	}
	if errClose := w.Close(); errClose != nil {
		t.Fatalf("gzip close: %v", errClose)
	}
	return buf.Bytes()
}

func zstdBody(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, errNew := zstd.NewWriter(&buf)
	if errNew != nil {
		t.Fatalf("zstd writer: %v", errNew)
	}
	if _, errWrite := w.Write(plain); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := w.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	return buf.Bytes()
}

func newBodyRequest(t *testing.T, body []byte, encoding string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

func TestReadRequestBodyDecodesSupportedEncodings(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	for _, tc := range []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "plain", encoding: "", body: plain},
		{name: "identity", encoding: "identity", body: plain},
		{name: "zstd", encoding: "zstd", body: zstdBody(t, plain)},
		{name: "gzip", encoding: "gzip", body: gzipBody(t, plain)},
		{name: "x-gzip", encoding: "x-gzip", body: gzipBody(t, plain)},
		{name: "gzip_upper", encoding: "GZIP", body: gzipBody(t, plain)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newBodyRequest(t, tc.body, tc.encoding)
			decoded, err := ReadRequestBody(c)
			if err != nil {
				t.Fatalf("ReadRequestBody: %v", err)
			}
			if !bytes.Equal(decoded, plain) {
				t.Fatalf("decoded = %q, want %q", decoded, plain)
			}
		})
	}
}

func TestReadRequestBodyGzipOverCap(t *testing.T) {
	plain := bytes.Repeat([]byte("a"), 1024)
	c, _ := newBodyRequest(t, gzipBody(t, plain), "gzip")
	c.Set(MaxRequestBodyBytesContextKey, int64(64))

	if _, err := ReadRequestBody(c); err == nil {
		t.Fatalf("expected max bytes error for oversized gzip body")
	} else {
		var maxErr *http.MaxBytesError
		if !asMaxBytesErr(err, &maxErr) {
			t.Fatalf("error = %v (%T), want *http.MaxBytesError", err, err)
		}
	}
}

func TestReadRequestBodyMalformedGzip(t *testing.T) {
	// A gzip-looking body that is not valid JSON and cannot be gunzipped must fail
	// rather than fall through as compressed bytes.
	c, _ := newBodyRequest(t, []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad}, "gzip")
	if _, err := ReadRequestBody(c); err == nil {
		t.Fatalf("expected decode error for malformed gzip body")
	}
}

func asMaxBytesErr(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if mbe, ok := err.(*http.MaxBytesError); ok {
			*target = mbe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
