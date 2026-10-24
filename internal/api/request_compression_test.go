package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func encodeIngressTestBody(t *testing.T, body []byte, encoding string) []byte {
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
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestServerCompressedRequestBodies(t *testing.T) {
	const limit = 256
	const model = "ingress-compression-unregistered-model"
	plain := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"input":"hello","prompt":"hello"}`)
	for _, requestLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("request-log=%t", requestLog), func(t *testing.T) {
			cfg := parseTestServerConfigYAML(t, fmt.Sprintf("config-version: 8\nserver:\n  max-request-body-bytes: %d\naccess:\n  api-keys:\n    - test-key\nobservability:\n  logs:\n    request-log: %t\n", limit, requestLog))
			server := newTestServerWithConfig(t, cfg)
			for _, endpoint := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/responses/compact"} {
				for _, encoding := range []string{"gzip", "zstd"} {
					for _, chunked := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/chunked=%t", endpoint, encoding, chunked), func(t *testing.T) {
							for _, variant := range []string{"valid", "stale header", "decoded limit", "wire limit", "truncated", "corrupt"} {
								if variant == "corrupt" && encoding != "gzip" {
									continue
								}
								t.Run(variant, func(t *testing.T) {
									body := plain
									status := http.StatusBadRequest
									message := "unknown provider for model " + model
									switch variant {
									case "decoded limit":
										body = []byte(`{"input":"` + strings.Repeat("x", limit*4) + `"}`)
										status = http.StatusRequestEntityTooLarge
										message = "request body too large"
									case "wire limit":
										status = http.StatusRequestEntityTooLarge
										message = "request body too large"
									case "truncated", "corrupt":
										message = "Invalid request:"
									}
									if variant != "stale header" {
										body = encodeIngressTestBody(t, body, encoding)
									}
									if variant == "wire limit" {
										body = bytes.Repeat([]byte("x"), limit*4)
									}
									if variant == "truncated" {
										body = body[:len(body)-3]
									}
									if variant == "corrupt" {
										body[len(body)-8] ^= 0xff
									}
									counter := &countingBodyReader{reader: bytes.NewReader(body)}
									req := httptest.NewRequest(http.MethodPost, endpoint, io.NopCloser(counter))
									req.ContentLength = int64(len(body))
									if chunked {
										req.ContentLength = -1
									}
									req.Header.Set("Content-Type", "application/json")
									req.Header.Set("Content-Encoding", encoding)
									req.Header.Set("Authorization", "Bearer test-key")
									recorder := httptest.NewRecorder()
									server.engine.ServeHTTP(recorder, req)
									if recorder.Code != status || !strings.Contains(recorder.Body.String(), message) {
										t.Fatalf("status=%d want=%d body=%s want message=%q", recorder.Code, status, recorder.Body.String(), message)
									}
									if read := counter.read.Load(); read > limit+1 {
										t.Fatalf("wire bytes read=%d want <=%d", read, limit+1)
									}
									if variant == "wire limit" && !chunked && counter.read.Load() != 0 {
										t.Fatalf("oversized known-length body was read: %d", counter.read.Load())
									}
								})
							}
						})
					}
				}
			}
		})
	}
}
