//go:build forkbench

package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var transportBenchPKI struct {
	sync.Once
	certificate tls.Certificate
	roots       *x509.CertPool
}

func transportBenchCertificate() (tls.Certificate, *x509.CertPool) {
	transportBenchPKI.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fork transport test CA"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, IsCA: true, BasicConstraintsValid: true,
			DNSNames: []string{"chatgpt.com", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		private, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			panic(err)
		}
		certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
		if err != nil {
			panic(err)
		}
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			panic(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(parsed)
		// The forkbench process uses a private CA, never disabled certificate verification.
		// x509's fallback roots require an explicit process-wide benchmark opt-in.
		x509.SetFallbackRoots(roots)
		transportBenchPKI.certificate, transportBenchPKI.roots = certificate, roots
	})
	return transportBenchPKI.certificate, transportBenchPKI.roots
}

type transportBenchStats struct {
	connections atomic.Int64
	handshakes  atomic.Int64
	upgrades    atomic.Int64
	requests    atomic.Int64
	wireBytes   atomic.Int64
	active      atomic.Int64
	peak        atomic.Int64
}

type transportBenchConn struct {
	net.Conn
	stats *transportBenchStats
	once  sync.Once
}

func (c *transportBenchConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.stats.wireBytes.Add(int64(n))
	return n, err
}
func (c *transportBenchConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.stats.wireBytes.Add(int64(n))
	return n, err
}
func (c *transportBenchConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.stats.active.Add(-1) })
	return err
}

type transportBenchListener struct {
	net.Listener
	stats *transportBenchStats
}

func (l transportBenchListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.stats.connections.Add(1)
	active := l.stats.active.Add(1)
	for peak := l.stats.peak.Load(); active > peak; peak = l.stats.peak.Load() {
		if l.stats.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	return &transportBenchConn{Conn: conn, stats: l.stats}, nil
}

type transportBenchFixture struct {
	server *httptest.Server
	proxy  *httptest.Server
	stats  transportBenchStats
	gateMu sync.Mutex
	gate   chan struct{}
	want   int
	seen   int
}

func newTransportBenchFixture(b *testing.B) *transportBenchFixture {
	certificate, _ := transportBenchCertificate()
	fixture := &transportBenchFixture{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				b.Errorf("upgrade: %v", err)
				return
			}
			fixture.stats.upgrades.Add(1)
			defer conn.Close()
			for {
				_, reader, errRead := conn.NextReader()
				if errRead != nil {
					return
				}
				if _, errRead = io.Copy(io.Discard, reader); errRead != nil {
					return
				}
				requestID := fixture.beforeResponse()
				for _, event := range transportBenchEvents(requestID) {
					if err = conn.WriteMessage(websocket.TextMessage, event); err != nil {
						return
					}
				}
			}
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			b.Errorf("HTTP body: %v", err)
			return
		}
		requestID := fixture.beforeResponse()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range transportBenchEvents(requestID) {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	fixture.server.EnableHTTP2 = true
	fixture.server.Listener = transportBenchListener{Listener: fixture.server.Listener, stats: &fixture.stats}
	fixture.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		fixture.stats.handshakes.Add(1)
		return nil, nil
	}}
	fixture.server.StartTLS()
	fixture.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		upstream, err := net.Dial("tcp", fixture.server.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = buffered.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		client.Close()
		<-done
	}))
	b.Cleanup(func() { fixture.proxy.Close(); fixture.server.Close() })
	return fixture
}

func (f *transportBenchFixture) beforeResponse() int64 {
	requestID := f.stats.requests.Add(1)
	f.gateMu.Lock()
	gate := f.gate
	if gate != nil {
		f.seen++
		if f.seen == f.want {
			close(gate)
			f.gate = nil
		}
	}
	f.gateMu.Unlock()
	if gate != nil {
		<-gate
	}
	return requestID
}

func (f *transportBenchFixture) abortBarrier() {
	f.gateMu.Lock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
	f.gateMu.Unlock()
}

func transportBenchEvents(requestID int64) [][]byte {
	events := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_bench","model":"gpt-5.4","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_bench","type":"message","role":"assistant","content":[]}}`),
		[]byte(`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`),
		[]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"synthetic benchmark output"}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_bench","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic benchmark output","annotations":[]}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_bench","model":"gpt-5.4","status":"completed","output":[{"id":"msg_bench","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic benchmark output","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`),
	}
	for index, event := range events {
		event = bytes.ReplaceAll(event, []byte("resp_bench"), []byte(fmt.Sprintf("resp_bench_%d", requestID)))
		events[index] = bytes.ReplaceAll(event, []byte("msg_bench"), []byte(fmt.Sprintf("msg_bench_%d", requestID)))
	}
	return events
}

type transportBenchExecutor interface {
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

func BenchmarkCodexHTTPTransportMatrix(b *testing.B) {
	level := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	defer log.SetLevel(level)
	if !strings.Contains(os.Getenv("GODEBUG"), "x509usefallbackroots=1") {
		b.Fatal("Private benchmark CA requires GODEBUG=x509usefallbackroots=1; run the documented forkbench command")
	}
	for _, size := range []int{1024, 64 << 10, 1 << 20, 8 << 20} {
		for _, concurrency := range []int{1, 4, 8, 16} {
			for _, headers := range []string{"stable", "changing"} {
				for _, mode := range []string{"utls-http", "native-http", "ephemeral-ws", "pooled-ws"} {
					b.Run(fmt.Sprintf("bytes=%d/c=%d/headers=%s/%s", size, concurrency, headers, mode), func(b *testing.B) {
						fixture := newTransportBenchFixture(b)
						cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
						cfg.ProxyURL = fixture.proxy.URL
						auth := &cliproxyauth.Auth{ID: "synthetic-bench-account", Provider: "codex", Attributes: map[string]string{
							"api_key": "synthetic-test-only", "base_url": "https://chatgpt.com/backend-api", "websockets": "true"}}
						ctx := b.Context()
						var executor transportBenchExecutor
						switch mode {
						case "utls-http":
							executor = NewCodexExecutor(cfg)
						case "native-http":
							cfg.ProxyURL = ""
							_, roots := transportBenchCertificate()
							proxyURL, err := url.Parse(fixture.proxy.URL)
							if err != nil {
								b.Fatal(err)
							}
							transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, Proxy: http.ProxyURL(proxyURL), MaxIdleConns: 32, MaxIdleConnsPerHost: 32, ForceAttemptHTTP2: true}
							b.Cleanup(transport.CloseIdleConnections)
							ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
							executor = NewCodexExecutor(cfg)
						case "ephemeral-ws":
							ws := NewCodexWebsocketsExecutor(cfg)
							b.Cleanup(func() { ws.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
							executor = ws
						case "pooled-ws":
							cfg.Codex.HTTPWebsockets = true
							ws := NewCodexAutoExecutor(cfg)
							b.Cleanup(func() { ws.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
							executor = ws
						}
						payload, err := json.Marshal(map[string]any{"model": "gpt-5.4", "input": strings.Repeat("x", size), "stream": true})
						if err != nil {
							b.Fatal(err)
						}
						request := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}
						ttfeSamples := make([]int64, b.N)
						completionSamples := make([]int64, b.N)
						var untimedTTFE atomic.Int64
						type preparedCall struct {
							context context.Context
							request cliproxyexecutor.Request
							options cliproxyexecutor.Options
						}
						prepare := func(index int64) preparedCall {
							headersForCall := http.Header{}
							if headers == "changing" {
								headersForCall.Set("X-Client-Request-Id", fmt.Sprint(index))
							}
							opts := cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: headersForCall,
								Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "synthetic-benchmark"}}
							enrichedRequest, enrichedOptions := coresession.Enrich(request, opts)
							identity := cliproxyauth.CanonicalSessionID(headersForCall, enrichedRequest.Payload, enrichedOptions.Metadata)
							if identity != "" {
								enrichedOptions.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = identity
							}
							return preparedCall{context: util.WithSessionID(ctx, identity), request: enrichedRequest, options: enrichedOptions}
						}
						prepared := make([]preparedCall, b.N)
						identityStarted := time.Now()
						for index := range prepared {
							prepared[index] = prepare(int64(index))
						}
						identitySetup := time.Since(identityStarted).Nanoseconds()
						call := func(entry preparedCall, index int64, timed bool) error {
							started := time.Now()
							stream, err := executor.ExecuteStream(entry.context, auth.Clone(), entry.request, entry.options)
							if err != nil {
								return err
							}
							generated := false
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									return chunk.Err
								}
								for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
									if !bytes.HasPrefix(line, []byte("data:")) {
										continue
									}
									data := bytes.TrimSpace(line[5:])
									if !generated && gjson.GetBytes(data, "type").String() == "response.output_text.delta" {
										generated = true
										if timed {
											ttfeSamples[index] = time.Since(started).Nanoseconds()
										} else {
											untimedTTFE.Store(time.Since(started).Nanoseconds())
										}
									}
								}
							}
							if !generated {
								return fmt.Errorf("no generated output")
							}
							if timed {
								completionSamples[index] = time.Since(started).Nanoseconds()
							}
							return nil
						}
						coldCall := prepare(-1000000)
						coldStart := time.Now()
						if err := call(coldCall, -1000000, false); err != nil {
							b.Fatal(err)
						}
						coldComplete, coldTTFE := time.Since(coldStart).Nanoseconds(), untimedTTFE.Load()
						// Warm all concurrent request lanes with a barrier, not a timing sleep.
						fixture.gateMu.Lock()
						fixture.gate, fixture.want, fixture.seen = make(chan struct{}), concurrency, 0
						fixture.gateMu.Unlock()
						var group sync.WaitGroup
						var errorMu sync.Mutex
						var failed error
						launch := func(index int64, timed bool) {
							defer group.Done()
							if err := call(prepare(index), index, timed); err != nil {
								errorMu.Lock()
								failed = err
								errorMu.Unlock()
								fixture.abortBarrier()
							}
						}
						for index := 0; index < concurrency; index++ {
							group.Add(1)
							go launch(int64(-index-1), false)
						}
						group.Wait()
						if failed != nil {
							b.Fatal(failed)
						}
						beforeConnections, beforeHandshakes := fixture.stats.connections.Load(), fixture.stats.handshakes.Load()
						beforeUpgrades, beforeBytes := fixture.stats.upgrades.Load(), fixture.stats.wireBytes.Load()
						fixture.stats.peak.Store(fixture.stats.active.Load())
						var index atomic.Int64
						b.ReportAllocs()
						b.SetBytes(int64(size))
						b.ResetTimer()
						for worker := 0; worker < concurrency; worker++ {
							group.Add(1)
							go func() {
								defer group.Done()
								for {
									next := index.Add(1) - 1
									if next >= int64(b.N) {
										return
									}
									if err := call(prepared[next], next, true); err != nil {
										errorMu.Lock()
										failed = err
										errorMu.Unlock()
										return
									}
								}
							}()
						}
						group.Wait()
						b.StopTimer()
						if failed != nil {
							b.Fatal(failed)
						}
						ops := float64(b.N)
						b.ReportMetric(float64(identitySetup)/ops, "identity-setup-ns/op")
						b.ReportMetric(float64(coldTTFE), "cold-ttfe-ns")
						b.ReportMetric(float64(coldComplete), "cold-complete-ns")
						for name, samples := range map[string][]int64{"ttfe": ttfeSamples, "complete": completionSamples} {
							sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
							b.ReportMetric(float64(samples[(len(samples)-1)/2]), name+"-p50-ns")
							b.ReportMetric(float64(samples[(95*len(samples)+99)/100-1]), name+"-p95-ns")
						}
						b.ReportMetric(float64(fixture.stats.connections.Load()-beforeConnections)/ops, "tcp/op")
						b.ReportMetric(float64(fixture.stats.handshakes.Load()-beforeHandshakes)/ops, "tls/op")
						b.ReportMetric(float64(fixture.stats.upgrades.Load()-beforeUpgrades)/ops, "upgrade/op")
						b.ReportMetric(float64(fixture.stats.wireBytes.Load()-beforeBytes)/ops, "wire-B/op")
						b.ReportMetric(float64(fixture.stats.peak.Load()), "peak-conns")
						if seconds := b.Elapsed().Seconds(); seconds > 0 {
							b.ReportMetric(ops/seconds, "requests/s")
						}
					})
				}
			}
		}
	}
}
