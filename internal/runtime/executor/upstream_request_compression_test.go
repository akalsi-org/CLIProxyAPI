package executor

import (
	"bytes"
	"compress/gzip"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClaudeUpstreamRequestEncodedRejectionFallback(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			var encoded bytes.Buffer
			var writer io.WriteCloser
			switch coding {
			case "gzip":
				writer = gzip.NewWriter(&encoded)
			case "br":
				writer = brotli.NewWriter(&encoded)
			case "zstd":
				encoder, err := zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				writer = encoder
			}
			_, _ = io.WriteString(writer, `{"error":{"code":"unsupported_content_encoding"}}`)
			_ = writer.Close()
			body := []byte(strings.Repeat("final JSON history ", 4096))
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			calls := 0
			client := &http.Client{Transport: compressionRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 415, Header: http.Header{"Content-Encoding": {coding}}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
				}
				if r.Header.Get("Content-Encoding") != "" {
					t.Fatal("fallback still compressed")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: preserved\n\n"))}, nil
			})}
			resp, err := doClaudeUpstreamRequest(helps.WithRequestCompression(client, req, body, decodeResponseBody), req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if calls != 2 || resp.StatusCode != 200 {
				t.Fatalf("encoded 415 did not fallback: calls=%d status=%d", calls, resp.StatusCode)
			}
		})
	}
}

type compressionRoundTripper func(*http.Request) (*http.Response, error)

func (f compressionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeUpstreamRequestCompressesFinalJSON(t *testing.T) {
	body := []byte(`{"model":"test","messages":[{"role":"user","content":"` + strings.Repeat("deterministic request history ", 4096) + `"}]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	client := &http.Client{Transport: compressionRoundTripper(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(gz)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, body) {
			t.Fatal("compression changed finalized JSON")
		}
		if r.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
			t.Fatal("response negotiation changed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: unchanged\n\n")), Request: r}, nil
	})}
	resp, err := doClaudeUpstreamRequest(helps.WithRequestCompression(client, req, body, decodeResponseBody), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
}
