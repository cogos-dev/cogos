package sdk

import (
	"errors"
	"fmt"
)

// Sentinel errors for common failure modes.
// Use errors.Is() to check for these.
var (
	// ErrNotFound indicates the requested resource does not exist.
	ErrNotFound = errors.New("resource not found")

	// ErrInvalidURI indicates the URI could not be parsed or is malformed.
	ErrInvalidURI = errors.New("invalid URI")

	// ErrUnknownNamespace indicates the URI namespace has no registered projector.
	ErrUnknownNamespace = errors.New("unknown namespace")

	// ErrIncoherent indicates the workspace state has drifted from canonical.
	ErrIncoherent = errors.New("workspace incoherent")

	// ErrNotConnected indicates an operation was attempted without a kernel connection.
	ErrNotConnected = errors.New("not connected to workspace")

	// ErrReadOnly indicates a mutation was attempted on a read-only projector.
	ErrReadOnly = errors.New("projector is read-only")

	// ErrValidation indicates a cogdoc or mutation failed validation.
	ErrValidation = errors.New("validation failed")

	// ErrWorkspaceNotFound indicates no .cog directory was found.
	ErrWorkspaceNotFound = errors.New("workspace not found")

	// ErrVersionMismatch indicates SDK version is incompatible with workspace.
	ErrVersionMismatch = errors.New("version mismatch")
)

// SDKError provides structured error information for debugging.
// It wraps underlying errors with operation context and recovery hints.
type SDKError struct {
	// Op is the operation that failed (e.g., "Resolve", "Mutate", "Connect").
	Op string

	// URI is the URI being operated on, if applicable.
	URI string

	// Path is the filesystem path, if applicable.
	Path string

	// Cause is the underlying error.
	Cause error

	// Recover is a human-readable suggestion for recovery.
	Recover string
}

// Error implements the error interface.
func (e *SDKError) Error() string {
	if e.URI != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.URI, e.Cause)
	}
	if e.Path != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Cause)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Cause)
}

// Unwrap returns the underlying error for errors.Is/As.
func (e *SDKError) Unwrap() error {
	return e.Cause
}

// Is checks if target matches this error's cause.
func (e *SDKError) Is(target error) bool {
	return errors.Is(e.Cause, target)
}

// NewError creates a new SDKError with the given operation and cause.
func NewError(op string, cause error) *SDKError {
	return &SDKError{Op: op, Cause: cause}
}

// NewURIError creates a new SDKError for a URI operation.
func NewURIError(op, uri string, cause error) *SDKError {
	return &SDKError{Op: op, URI: uri, Cause: cause}
}

// NewPathError creates a new SDKError for a filesystem operation.
func NewPathError(op, path string, cause error) *SDKError {
	return &SDKError{Op: op, Path: path, Cause: cause}
}

// WithRecover adds a recovery suggestion to an error.
func (e *SDKError) WithRecover(hint string) *SDKError {
	e.Recover = hint
	return e
}

// NotFoundError returns an SDKError wrapping ErrNotFound.
func NotFoundError(op, uri string) *SDKError {
	return &SDKError{
		Op:    op,
		URI:   uri,
		Cause: ErrNotFound,
	}
}

// InvalidURIError returns an SDKError wrapping ErrInvalidURI with details.
func InvalidURIError(uri, reason string) *SDKError {
	return &SDKError{
		Op:    "ParseURI",
		URI:   uri,
		Cause: fmt.Errorf("%w: %s", ErrInvalidURI, reason),
	}
}

// IncoherentError returns an SDKError wrapping ErrIncoherent.
func IncoherentError(driftedFiles []string) *SDKError {
	return &SDKError{
		Op:      "CheckCoherence",
		Cause:   ErrIncoherent,
		Recover: "Run './scripts/cog coherence baseline' to reset",
	}
}
