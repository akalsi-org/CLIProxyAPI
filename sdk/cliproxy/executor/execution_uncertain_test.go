package executor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
)

type uncertainTestError struct{ cause error }

func (e *uncertainTestError) Error() string        { return `{"error":{"message":"upstream disconnected"}}` }
func (e *uncertainTestError) Unwrap() error        { return e.cause }
func (e *uncertainTestError) StatusCode() int      { return http.StatusServiceUnavailable }
func (e *uncertainTestError) Headers() http.Header { return http.Header{"X-Test": {"preserved"}} }

func TestExecutionUncertainRetainsCauseAndHTTPInterfaces(t *testing.T) {
	cause := &uncertainTestError{cause: io.ErrUnexpectedEOF}
	marked := MarkExecutionUncertain(cause)
	if !IsExecutionUncertain(marked) || !IsExecutionUncertain(fmt.Errorf("wrapped: %w", marked)) {
		t.Fatal("marker was lost")
	}
	if marked.Error() != cause.Error() || !errors.Is(marked, io.ErrUnexpectedEOF) || !errors.Is(marked, cause) {
		t.Fatal("cause or payload was lost")
	}
	if marked.(StatusError).StatusCode() != http.StatusServiceUnavailable {
		t.Fatal("status was lost")
	}
	if marked.(interface{ Headers() http.Header }).Headers().Get("X-Test") != "preserved" {
		t.Fatal("headers were lost")
	}
	if MarkExecutionUncertain(marked) != marked {
		t.Fatal("marker is not idempotent")
	}
	if MarkExecutionUncertain(nil) != nil || IsExecutionUncertain(nil) || IsExecutionUncertain(cause) {
		t.Fatal("unmarked error was classified as uncertain")
	}
	plain := MarkExecutionUncertain(io.EOF)
	if plain.(StatusError).StatusCode() != 0 || plain.(interface{ Headers() http.Header }).Headers() != nil {
		t.Fatal("plain error acquired HTTP metadata")
	}
}
