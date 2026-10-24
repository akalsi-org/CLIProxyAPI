package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func codexWSCompressionFixture() []byte {
	var history strings.Builder
	for i := range 400 {
		fmt.Fprintf(&history, "Turn %d: inspect module_%d.go, preserve cancellation and payload semantics. %s\n", i, i%19, strings.Repeat("Synthetic source review context; no user transcript or credential data. ", 3))
	}
	payload, _ := json.Marshal(map[string]any{"type": "response.create", "model": "fixture", "input": history.String()})
	return payload
}

func codexWSCompressionChunkedFixture() []byte {
	payload := codexWSCompressionFixture()
	attachment := make([]byte, 96<<10)
	_, _ = mathrand.New(mathrand.NewSource(6335)).Read(attachment)
	return append(payload[:len(payload)-1], []byte(`,"fixture_attachment":"`+base64.StdEncoding.EncodeToString(attachment)+`"}`)...)
}

type codexWSFrameObservation struct {
	Compressed   bool `json:"compressed"`
	WireBytes    int  `json:"wire_bytes"`
	DecodedBytes int  `json:"decoded_bytes"`
	Frames       int  `json:"frames"`
}

// This fixture observes RSV1 before decoding, including fragmented client messages.
func codexWSCompressionHandler(w http.ResponseWriter, r *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	hash := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n", base64.StdEncoding.EncodeToString(hash[:]))
	if r.URL.Query().Get("deflate") != "off" {
		_, _ = io.WriteString(rw, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n")
	}
	_, _ = io.WriteString(rw, "\r\n")
	if rw.Flush() != nil {
		return
	}
	expected := codexWSCompressionFixture()
	if r.URL.Query().Get("fixture") == "chunked" {
		expected = codexWSCompressionChunkedFixture()
	}
	for {
		observation, err := readCodexWSCompressionFrame(rw.Reader, expected)
		if err != nil {
			return
		}
		ack, _ := json.Marshal(observation)
		if len(ack) > 125 {
			return
		}
		_, _ = rw.Write([]byte{0x81, byte(len(ack))})
		_, _ = rw.Write(ack)
		if rw.Flush() != nil {
			return
		}
	}
}

func readCodexWSCompressionFrame(reader *bufio.Reader, expected []byte) (codexWSFrameObservation, error) {
	var observation codexWSFrameObservation
	var payload bytes.Buffer
	for first := true; ; first = false {
		var header [2]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return observation, err
		}
		if header[0]&0x0f == websocket.CloseMessage {
			return observation, io.EOF
		}
		if first {
			if header[0]&0x0f != websocket.TextMessage {
				return observation, fmt.Errorf("first frame is not text")
			}
			observation.Compressed = header[0]&0x40 != 0
		} else if header[0]&0x0f != 0 || header[0]&0x40 != 0 {
			return observation, fmt.Errorf("invalid continuation opcode or RSV1")
		}
		if header[0]&0x30 != 0 {
			return observation, fmt.Errorf("unexpected RSV2 or RSV3")
		}
		observation.Frames++
		size := uint64(header[1] & 0x7f)
		observation.WireBytes += 2
		switch size {
		case 126:
			var extended [2]byte
			if _, err := io.ReadFull(reader, extended[:]); err != nil {
				return observation, err
			}
			size = uint64(binary.BigEndian.Uint16(extended[:]))
			observation.WireBytes += 2
		case 127:
			var extended [8]byte
			if _, err := io.ReadFull(reader, extended[:]); err != nil {
				return observation, err
			}
			size = binary.BigEndian.Uint64(extended[:])
			observation.WireBytes += 8
		}
		if header[1]&0x80 == 0 || size > 2<<20 || uint64(payload.Len())+size > 2<<20 {
			return observation, fmt.Errorf("invalid or oversized client frame")
		}
		var mask [4]byte
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return observation, err
		}
		fragment := make([]byte, int(size))
		if _, err := io.ReadFull(reader, fragment); err != nil {
			return observation, err
		}
		for i := range fragment {
			fragment[i] ^= mask[i%4]
		}
		_, _ = payload.Write(fragment)
		observation.WireBytes += 4 + int(size)
		if header[0]&0x80 != 0 {
			break
		}
	}
	decoded := payload.Bytes()
	if observation.Compressed {
		// Restore the RFC 7692 tail and final block, as Gorilla's reader does.
		decoder := flate.NewReader(io.MultiReader(bytes.NewReader(decoded), strings.NewReader("\x00\x00\xff\xff\x01\x00\x00\xff\xff")))
		var err error
		decoded, err = io.ReadAll(io.LimitReader(decoder, (2<<20)+1))
		_ = decoder.Close()
		if err != nil {
			return observation, err
		}
	}
	if len(decoded) > 2<<20 || !json.Valid(decoded) || !bytes.Equal(decoded, expected) {
		return observation, fmt.Errorf("decoded frame changed fixture JSON")
	}
	observation.DecodedBytes = len(decoded)
	return observation, nil
}

func TestCodexWebsocketWriteCompressionFrames(t *testing.T) {
	if os.Getenv("CLI_PROXY_WS_COMPRESSION_CHILD") == "1" {
		for _, bridge := range []bool{false, true} {
			for _, tc := range []struct {
				url        string
				compressed bool
			}{
				{"wss://chatgpt.com/backend-api/codex/responses", true},
				{"wss://chatgpt.com:443/backend-api/codex/v1/responses", true},
				{"wss://gateway.example/backend-api/codex/responses", false},
				{"wss://chatgpt.com/v1/responses", false},
				{"wss://chatgpt.com/backend-api/codex/responses?deflate=off", false},
			} {
				t.Run(fmt.Sprintf("bridge=%v/%s", bridge, tc.url), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if bridge {
						ctx = context.WithValue(ctx, codexHTTPWebsocketContextKey{}, &codexHTTPWebsocketBridge{})
					}
					executor := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: os.Getenv("CLI_PROXY_WS_COMPRESSION_PROXY")}})
					separator := "?"
					if strings.Contains(tc.url, "?") {
						separator = "&"
					}
					conn, closer, _, err := executor.dialCodexWebsocket(ctx, nil, tc.url+separator+"fixture=chunked", nil)
					if err != nil {
						t.Fatal(err)
					}
					defer closer.Close()
					_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
					payload := codexWSCompressionChunkedFixture()
					if len(payload) <= 64<<10 {
						t.Fatal("fixture must exceed two production chunks")
					}
					session := &codexWebsocketSession{sessionID: "compression-fixture"}
					for message := range 2 {
						chunks := 0
						expectedChunks := (len(payload) + codexWebsocketWriteChunkSize - 1) / codexWebsocketWriteChunkSize
						testWebsocketWriteChunkHook = func(index, total int) {
							if index != chunks || total != expectedChunks {
								t.Errorf("chunk index=%d total=%d, want index=%d total=%d", index, total, chunks, expectedChunks)
							}
							chunks++
						}
						t.Cleanup(func() { testWebsocketWriteChunkHook = nil })
						if err := writeWebsocketPayloadMessage("codex", session, conn, payload); err != nil {
							t.Fatal(err)
						}
						if chunks != expectedChunks {
							t.Fatalf("production chunk count=%d, want %d", chunks, expectedChunks)
						}
						_, ack, err := conn.ReadMessage()
						if err != nil {
							t.Fatal(err)
						}
						var observation codexWSFrameObservation
						if err := json.Unmarshal(ack, &observation); err != nil {
							t.Fatal(err)
						}
						if observation.Compressed != tc.compressed {
							t.Fatalf("message %d RSV1=%v, want %v", message, observation.Compressed, tc.compressed)
						}
						if observation.DecodedBytes != len(payload) {
							t.Fatal("decoded payload changed")
						}
						// A masked frame header uses at most 14 bytes, excluding encoded payload.
						if observation.Frames <= 1 || observation.WireBytes-observation.Frames*14 <= 32<<10 {
							t.Fatalf("production upload did not fragment: frames=%d wire_bytes=%d", observation.Frames, observation.WireBytes)
						}
						t.Logf("message=%d compressed=%v chunks=%d frames=%d plain_bytes=%d wire_bytes=%d", message, observation.Compressed, chunks, observation.Frames, len(payload), observation.WireBytes)
					}
				})
			}
		}
		return
	}
	rootFile, certificate := codexWSCompressionCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(codexWSCompressionHandler))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	defer upstream.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "chatgpt.com:443" && r.Host != "gateway.example:443") {
			http.Error(w, "unexpected CONNECT target", http.StatusForbidden)
			return
		}
		target, err := net.DialTimeout("tcp", strings.TrimPrefix(upstream.URL, "https://"), 2*time.Second)
		if err != nil {
			http.Error(w, "local fixture unavailable", http.StatusBadGateway)
			return
		}
		defer target.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(30 * time.Second))
		_ = target.SetDeadline(time.Now().Add(30 * time.Second))
		_, _ = io.WriteString(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if rw.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(target, rw); _ = target.Close(); close(done) }()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		<-done
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexWebsocketWriteCompressionFrames$", "-test.count=1", "-test.timeout=35s", "-test.v")
	command.Env = append(os.Environ(), "CLI_PROXY_WS_COMPRESSION_CHILD=1", "CLI_PROXY_WS_COMPRESSION_PROXY="+proxy.URL, "SSL_CERT_FILE="+rootFile, "SSL_CERT_DIR="+t.TempDir(), "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "NO_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=", "no_proxy=")
	output, err := command.CombinedOutput()
	t.Logf("child frame observations:\n%s", output)
	if err != nil {
		t.Fatalf("compression child failed: %v", err)
	}
}

func codexWSCompressionCertificate(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "temporary WebSocket fixture"}, DNSNames: []string{"chatgpt.com", "gateway.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(t.TempDir(), "fixture-root.pem")
	if err := os.WriteFile(rootFile, certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	return rootFile, certificate
}

func BenchmarkCodexWebsocketCompressionLocal(b *testing.B) {
	for _, compressed := range []bool{false, true} {
		b.Run(fmt.Sprintf("compressed=%v", compressed), func(b *testing.B) {
			upstream := httptest.NewServer(http.HandlerFunc(codexWSCompressionHandler))
			defer upstream.Close()
			dialer := websocket.Dialer{EnableCompression: true, WriteBufferSize: 32 << 10, HandshakeTimeout: 5 * time.Second}
			conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			conn.EnableWriteCompression(compressed)
			payload := codexWSCompressionFixture()
			var observation codexWSFrameObservation
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					b.Fatal(err)
				}
				_, ack, err := conn.ReadMessage()
				if err != nil {
					b.Fatal(err)
				}
				if err := json.Unmarshal(ack, &observation); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(payload)), "plain-bytes")
			b.ReportMetric(float64(observation.WireBytes), "frame-bytes")
		})
	}
}
