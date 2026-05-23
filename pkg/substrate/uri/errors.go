package uri

import (
	"errors"
	"fmt"
)

// Sentinel errors for URI operations.
var (
	// ErrInvalidURI indicates the URI could not be parsed or is malformed.
	ErrInvalidURI = errors.New("invalid URI")

	// ErrUnknownNamespace indicates the URI namespace is not recognized.
	ErrUnknownNamespace = errors.New("unknown namespace")
)

// Error provides structured error information for URI operations.
type Error struct {
	// Op is the operation that failed (e.g. "Parse").
	Op string
	// URI is the URI being operated on.
	URI string
	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.URI != "" {
		return fmt.Sprintf("%s %q: %v", e.Op, e.URI, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

// Unwrap returns the underlying error for errors.Is/As.
func (e *Error) Unwrap() error {
	return e.Err
}
