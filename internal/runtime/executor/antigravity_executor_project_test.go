package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const antigravityProjectTestPayload = `{"project":"payload-project","request":{"project":"nested-project","contents":[{"role":"user","parts":[{"text":"synthetic diagnostic"}]}]}}`
const antigravityProjectTestResponse = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`

func antigravityProjectTestAuth(id string, stored bool) *cliproxyauth.Auth {
	auth := antigravityCoolingTestAuth(id, "https://synthetic.invalid")
	if stored {
		auth.Metadata["project_id"] = "stored-project"
	} else {
		delete(auth.Metadata, "project_id")
	}
	auth.Metadata["unrelated"] = "keep"
	return auth
}

func TestAntigravityProjectOverrideSelection(t *testing.T) {
	for _, override := range []string{"", " \t\n", " default-cli-project "} {
		for _, stored := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("override=%q/stored=%v/stream=%v", override, stored, stream), func(t *testing.T) {
					cfg := &config.Config{Antigravity: config.AntigravityConfig{ProjectID: override}}
					executor := NewAntigravityExecutor(cfg)
					auth := antigravityProjectTestAuth(t.Name(), stored)
					auth.Attributes["header:X-Goog-User-Project"] = "quota-project-from-caller"
					before := auth.Clone()
					active := strings.TrimSpace(override) != ""
					if executor.ShouldPrepareRequestAuth(auth) != (!active && !stored) {
						t.Fatal("project preparation does not match the override and stored metadata")
					}
					req, err := executor.buildRequest(context.Background(), auth, "synthetic-token", "gemini-3.8-flash-high", []byte(antigravityProjectTestPayload), stream, "", "https://synthetic.invalid")
					if !active && !stored {
						if err == nil || !strings.Contains(err.Error(), "missing project_id") {
							t.Fatalf("missing-project behavior changed: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					_ = req.Body.Close()
					want := "stored-project"
					if active {
						want = "default-cli-project"
					}
					if got := gjson.GetBytes(body, "project").String(); got != want {
						t.Fatalf("envelope project = %q, want %q", got, want)
					}
					if got := gjson.GetBytes(body, "request.project").String(); got != "nested-project" {
						t.Fatalf("nested request.project changed: %q", got)
					}
					if req.Header.Get("X-Goog-User-Project") != "quota-project-from-caller" || req.Header.Get("Authorization") != "Bearer synthetic-token" {
						t.Fatal("project override changed quota-project or authentication headers")
					}
					if !reflect.DeepEqual(before.Metadata, auth.Metadata) || !reflect.DeepEqual(before.Attributes, auth.Attributes) {
						t.Fatal("generation project selection changed auth metadata or attributes")
					}
				})
			}
		}
	}
	var executor *AntigravityExecutor
	if _, err := executor.projectIDForRequest(context.Background(), nil, ""); err == nil {
		t.Fatal("missing configuration and auth must retain the missing-project error")
	}
}

func runAntigravityProjectGeneration(ctx context.Context, executor *AntigravityExecutor, auth *cliproxyauth.Auth, model string, stream bool) error {
	request := cliproxyexecutor.Request{Model: model, Payload: []byte(antigravityProjectTestPayload)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity, Stream: stream}
	if !stream {
		_, err := executor.Execute(ctx, auth, request, opts)
		return err
	}
	result, err := executor.ExecuteStream(ctx, auth, request, opts)
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

func TestAntigravityProjectOverrideGenerationPaths(t *testing.T) {
	for _, override := range []string{"", " \t\n", " default-cli-project "} {
		for _, stored := range []bool{false, true} {
			for _, model := range []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"} {
				for _, stream := range []bool{false, true} {
					for _, credits := range []bool{false, true} {
						t.Run(fmt.Sprintf("override=%q/stored=%v/model=%s/stream=%v/credits=%v", override, stored, model, stream, credits), func(t *testing.T) {
							cfg := &config.Config{Antigravity: config.AntigravityConfig{ProjectID: override}}
							cfg.QuotaExceeded.AntigravityCredits = credits
							executor := NewAntigravityExecutor(cfg)
							auth := antigravityProjectTestAuth(t.Name(), stored)
							before := auth.Clone()
							cliproxyauth.SetAntigravityCreditsHint(auth.ID, cliproxyauth.AntigravityCreditsHint{Known: true, Available: true, UpdatedAt: time.Now()})
							t.Cleanup(func() { cliproxyauth.SetAntigravityCreditsHint(auth.ID, cliproxyauth.AntigravityCreditsHint{}) })
							var calls atomic.Int32
							ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
								if req.URL.Path != antigravityGeneratePath && req.URL.Path != antigravityStreamPath {
									return nil, fmt.Errorf("unexpected acquisition request: %s", req.URL)
								}
								calls.Add(1)
								body, err := io.ReadAll(req.Body)
								if err != nil {
									return nil, err
								}
								want := "stored-project"
								if strings.TrimSpace(override) != "" {
									want = "default-cli-project"
								}
								if got := gjson.GetBytes(body, "project").String(); got != want {
									t.Errorf("generation project = %q, want %q", got, want)
								}
								if gjson.GetBytes(body, "enabledCreditTypes").Exists() != credits {
									t.Error("generation project override changed credits injection")
								}
								if got := req.Header.Get("X-Goog-User-Project"); got != "" {
									t.Errorf("override added a quota-project header: %q", got)
								}
								response := antigravityProjectTestResponse
								if req.URL.Path == antigravityStreamPath {
									response = "data: " + response + "\n\n"
								}
								return issue6199AntigravityJSONResponse(req, response), nil
							}))
							if credits {
								ctx = cliproxyauth.WithAntigravityCredits(ctx)
							}
							err := runAntigravityProjectGeneration(ctx, executor, auth, model, stream)
							if strings.TrimSpace(override) == "" && !stored {
								if err == nil || calls.Load() != 0 || !strings.Contains(err.Error(), "missing project_id") {
									t.Fatalf("missing-project behavior changed: calls=%d err=%v", calls.Load(), err)
								}
							} else if err != nil || calls.Load() != 1 {
								t.Fatalf("generation failed: calls=%d err=%v", calls.Load(), err)
							}
							if !reflect.DeepEqual(before.Metadata, auth.Metadata) || !reflect.DeepEqual(before.Attributes, auth.Attributes) {
								t.Fatal("generation changed stored project metadata")
							}
						})
					}
				}
			}
		}
	}
}

func TestAntigravityProjectOverridePreparation(t *testing.T) {
	for _, override := range []string{"", " \t\n", "default-cli-project"} {
		t.Run(fmt.Sprintf("override=%q", override), func(t *testing.T) {
			executor := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{ProjectID: override}})
			auth := antigravityProjectTestAuth(t.Name(), false)
			before := auth.Clone()
			var calls atomic.Int32
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.URL.String() != "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
					return nil, fmt.Errorf("unexpected acquisition request: %s", req.URL)
				}
				return issue6199AntigravityJSONResponse(req, `{"cloudaicompanionProject":"discovered-project"}`), nil
			}))
			updated, err := executor.PrepareRequestAuth(ctx, auth)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(override) != "" {
				if updated != nil || calls.Load() != 0 {
					t.Fatal("active override performed project preparation")
				}
			} else if calls.Load() != 1 || updated == nil || antigravityProjectIDFromAuth(updated) != "discovered-project" {
				t.Fatal("blank override did not retain project discovery")
			}
			if !reflect.DeepEqual(before.Metadata, auth.Metadata) || !reflect.DeepEqual(before.Attributes, auth.Attributes) {
				t.Fatal("project preparation changed the input auth")
			}
		})
	}
}

func TestAntigravityProjectOverrideDoesNotFallBackAfterError(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		for _, model := range []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("status=%d/model=%s/stream=%v", status, model, stream), func(t *testing.T) {
					cfg := &config.Config{RequestRetry: 3, Antigravity: config.AntigravityConfig{ProjectID: "default-cli-project"}}
					auth := antigravityProjectTestAuth(t.Name(), true)
					before := auth.Clone()
					var calls atomic.Int32
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						calls.Add(1)
						body, err := io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						if gjson.GetBytes(body, "project").String() != "default-cli-project" {
							t.Error("executor used an alternate project after rejection")
						}
						response := issue6199AntigravityJSONResponse(req, `{"error":{"message":"synthetic project rejection"}}`)
						response.StatusCode = status
						return response, nil
					}))
					if err := runAntigravityProjectGeneration(ctx, NewAntigravityExecutor(cfg), auth, model, stream); err == nil {
						t.Fatal("generation ignored the project rejection")
					}
					if calls.Load() != 1 || !reflect.DeepEqual(before.Metadata, auth.Metadata) || !reflect.DeepEqual(before.Attributes, auth.Attributes) {
						t.Fatal("project rejection retried or changed stored metadata")
					}
				})
			}
		}
	}
}

type antigravityProjectTestStore struct {
	mu    sync.Mutex
	saved *cliproxyauth.Auth
}

func (*antigravityProjectTestStore) List(context.Context) ([]*cliproxyauth.Auth, error) {
	return nil, nil
}
func (*antigravityProjectTestStore) Delete(context.Context, string) error { return nil }
func (s *antigravityProjectTestStore) Save(_ context.Context, auth *cliproxyauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = auth.Clone()
	return auth.ID, nil
}

func TestAntigravityProjectOverridePreservesRefreshAndPersistence(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(fmt.Sprintf("stored=%v", stored), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{Antigravity: config.AntigravityConfig{ProjectID: "default-cli-project"}}
				executor := NewAntigravityExecutor(cfg)
				auth := antigravityProjectTestAuth(t.Name(), stored)
				delete(auth.Attributes, "base_url")
				auth.Metadata["refresh_token"] = t.Name() + "-synthetic-refresh"
				auth.Metadata["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
				var tokenCalls, creditsCalls atomic.Int32
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					switch req.URL.String() {
					case "https://oauth2.googleapis.com/token":
						tokenCalls.Add(1)
						return issue6199AntigravityJSONResponse(req, `{"access_token":"refreshed-synthetic-token","expires_in":3600}`), nil
					case "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
						creditsCalls.Add(1)
						return issue6199AntigravityJSONResponse(req, `{"cloudaicompanionProject":"must-not-be-stored","paidTier":{"id":"synthetic-tier","availableCredits":[]}}`), nil
					default:
						return nil, fmt.Errorf("unexpected acquisition request: %s", req.URL)
					}
				}))
				store := &antigravityProjectTestStore{}
				manager := cliproxyauth.NewManager(store, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(executor)
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				updated, err := manager.ForceRefreshAuth(ctx, auth.ID)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if tokenCalls.Load() != 1 || creditsCalls.Load() != 1 {
					t.Fatalf("refresh calls: token=%d loadCodeAssist=%d, want one each", tokenCalls.Load(), creditsCalls.Load())
				}
				store.mu.Lock()
				persisted := store.saved.Clone()
				store.mu.Unlock()
				for _, snapshot := range []*cliproxyauth.Auth{updated, persisted} {
					if metaStringValue(snapshot.Metadata, "access_token") != "refreshed-synthetic-token" || snapshot.Metadata["unrelated"] != "keep" {
						t.Fatal("override disabled token refresh, persistence, or unrelated metadata preservation")
					}
					_, hasProject := snapshot.Metadata["project_id"]
					if hasProject != stored || (stored && snapshot.Metadata["project_id"] != "stored-project") {
						t.Fatal("token refresh changed stored project selection")
					}
				}
				antigravityCreditsHintRefreshByID.Delete(auth.ID)
			})
		})
	}
}

func TestAntigravityProjectOverrideHomeGeneration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var calls atomic.Int32
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if gjson.GetBytes(body, "project").String() != "default-cli-project" {
					t.Error("Home generation did not use its delivered project override")
				}
				response := antigravityProjectTestResponse
				if req.URL.Path == antigravityStreamPath {
					response = "data: " + response + "\n\n"
				}
				return issue6199AntigravityJSONResponse(req, response), nil
			})
			cfg := &config.Config{Antigravity: config.AntigravityConfig{ProjectID: "default-cli-project"}}
			cfg.Home.Enabled = true
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(NewAntigravityExecutor(cfg))
			manager.PublishHomeDispatch(&antigravityHomeModelCapabilityDispatcher{baseURL: "https://synthetic.invalid"}, executionregistry.New(), 1)
			local := antigravityProjectTestAuth("home-antigravity-3.8-auth", true)
			if _, err := manager.Register(context.Background(), local); err != nil {
				t.Fatal(err)
			}
			before, _ := manager.GetByID(local.ID)
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			request := cliproxyexecutor.Request{Model: "gemini-3.8-flash-high", Payload: []byte(antigravityProjectTestPayload)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity, Stream: stream}
			if stream {
				result, err := manager.ExecuteStream(ctx, []string{"antigravity"}, request, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := manager.Execute(ctx, []string{"antigravity"}, request, opts); err != nil {
				t.Fatal(err)
			}
			after, _ := manager.GetByID(local.ID)
			if calls.Load() != 1 || !reflect.DeepEqual(before, after) {
				t.Fatal("Home generation retried or changed same-ID local auth state")
			}
		})
	}
}
