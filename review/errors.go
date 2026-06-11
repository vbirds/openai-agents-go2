package review

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the review package. Match with errors.Is.
var (
	// ErrInvalidRequest indicates the Request was malformed (no input,
	// invalid schema JSON, ...).
	ErrInvalidRequest = errors.New("review: invalid request")

	// ErrInvalidSchema indicates the supplied output schema could not be
	// parsed or resolved as a JSON Schema.
	ErrInvalidSchema = errors.New("review: invalid output schema")

	// ErrInvalidOutput indicates the model failed to produce output that
	// validates against the schema after all retries were exhausted.
	ErrInvalidOutput = errors.New("review: model output did not conform to schema")
)

// OutputError wraps ErrInvalidOutput with the offending raw output so callers
// can log or inspect what the model actually produced.
type OutputError struct {
	// RawOutput is the last (non-conforming) model output.
	RawOutput string
	// Reason describes the validation failure.
	Reason error
}

// Error implements the error interface.
func (e *OutputError) Error() string {
	return fmt.Sprintf("%v: %v", ErrInvalidOutput, e.Reason)
}

// Unwrap allows errors.Is(err, ErrInvalidOutput).
func (e *OutputError) Unwrap() error { return ErrInvalidOutput }
