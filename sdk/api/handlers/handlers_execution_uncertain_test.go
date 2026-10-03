package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestExecuteStreamWithAuthManager_ExecutionUncertainStopsBootstrapReplay(t *testing.T) {
	for _, dropPayload := range []bool{false, true} {
		t.Run(fmt.Sprintf("dropped-payload=%t", dropPayload), func(t *testing.T) {
			cause := &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: `{"error":{"message":"ambiguous upstream execution"}}`}
			marked := fmt.Errorf("wrapped: %w", coreexecutor.MarkExecutionUncertain(cause))
			executor := &bootstrapStreamExecutor{stream: func(ctx context.Context, _ int) (*coreexecutor.StreamResult, error) {
				coreexecutor.MarkUpstreamAttempt(ctx)
				chunks := make(chan coreexecutor.StreamChunk, 2)
				if dropPayload {
					chunks <- coreexecutor.StreamChunk{Payload: []byte("drop")}
				}
				chunks <- coreexecutor.StreamChunk{Err: marked}
				close(chunks)
				return &coreexecutor.StreamResult{Chunks: chunks}, nil
			}}
			handler, manager := registerBootstrapExecutor(t, executor)
			manager.SetRetryConfig(5, 0, 6)
			if dropPayload {
				handler.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(_ context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
					return pluginapi.StreamChunkInterceptResponse{Body: cloneBytes(req.Body), DropChunk: true}
				}})
			}
			data, _, failures := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "bootstrap-model", []byte(`{"model":"bootstrap-model"}`), "")
			for payload := range data {
				t.Fatalf("unexpected payload %q", payload)
			}
			failureCount := 0
			for failure := range failures {
				if failure == nil {
					continue
				}
				failureCount++
				if failure.StatusCode != http.StatusServiceUnavailable || !errors.Is(failure.Error, cause) || !coreexecutor.IsExecutionUncertain(failure.Error) {
					t.Fatalf("uncertain failure lost its metadata: %+v", failure)
				}
			}
			if failureCount != 1 || executor.Calls() != 1 {
				t.Fatalf("failures=%d executor calls=%d, want one each", failureCount, executor.Calls())
			}
		})
	}
}
