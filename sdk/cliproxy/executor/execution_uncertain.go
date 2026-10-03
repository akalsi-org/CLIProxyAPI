package executor

import (
	"errors"
	"net/http"
)

// MarkExecutionUncertain prevents replay when upstream execution may have started.
// Use it for ambiguous send/read failures, not explicit upstream rejection.
// The returned error retains the cause, message, status, and response headers.
func MarkExecutionUncertain(err error) error {
	if err == nil || IsExecutionUncertain(err) {
		return err
	}
	return &executionUncertainError{cause: err}
}

// IsExecutionUncertain reports whether an error forbids replay, including wrapped errors.
func IsExecutionUncertain(err error) bool {
	var marked *executionUncertainError
	return errors.As(err, &marked) && marked != nil
}

type executionUncertainError struct {
	cause error
}

func (e *executionUncertainError) Error() string { return e.cause.Error() }
func (e *executionUncertainError) Unwrap() error { return e.cause }

func (e *executionUncertainError) StatusCode() int {
	var status interface{ StatusCode() int }
	if errors.As(e.cause, &status) {
		return status.StatusCode()
	}
	return 0
}

func (e *executionUncertainError) Headers() http.Header {
	var headers interface{ Headers() http.Header }
	if errors.As(e.cause, &headers) {
		return headers.Headers()
	}
	return nil
}
