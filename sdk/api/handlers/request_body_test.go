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
