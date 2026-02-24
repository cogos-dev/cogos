// retry.go provides error classification and retry constants.
//
// ClassifyError inspects error messages to determine whether an error is
// retryable. ClassifyHTTPError does the same for HTTP status codes. These are
// used by RunInferenceWithRetry to decide whether to retry, and by RunInference
// to tag responses with ErrorType for callers.
package harness

import (
	"strings"
	"time"
)

// Default retry configuration.
const (
	DefaultMaxRetries = 3
	DefaultTimeout    = 2 * time.Minute
	BaseRetryDelay    = time.Second
)

// ErrorType classifies inference errors for smart recovery
type ErrorType int

const (
	ErrorNone            ErrorType = iota
	ErrorRateLimit                 // 429 - retry with backoff
	ErrorContextOverflow           // Context too long - compress and retry
	ErrorAuth                      // Authentication failure - fail fast
	ErrorTransient                 // Transient failure - retry with backoff
	ErrorFatal                     // Fatal error - don't retry
)

// String returns human-readable error type
func (e ErrorType) String() string {
	switch e {
	case ErrorNone:
		return "none"
	case ErrorRateLimit:
		return "rate_limit"
	case ErrorContextOverflow:
		return "context_overflow"
	case ErrorAuth:
		return "auth"
	case ErrorTransient:
		return "transient"
	case ErrorFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// ClassifyError determines the error type from an error message
func ClassifyError(err error) ErrorType {
	if err == nil {
		return ErrorNone
	}
	errMsg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(errMsg, "429") || strings.Contains(errMsg, "rate limit") || strings.Contains(errMsg, "too many requests"):
		return ErrorRateLimit
	case strings.Contains(errMsg, "context_length") || strings.Contains(errMsg, "context length") || strings.Contains(errMsg, "too long"):
		return ErrorContextOverflow
	case strings.Contains(errMsg, "auth") || strings.Contains(errMsg, "unauthorized") || strings.Contains(errMsg, "401"):
		return ErrorAuth
	case strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "connection"):
		return ErrorTransient
	default:
		return ErrorTransient // Default to transient for unknown errors
	}
}

// ClassifyHTTPError maps HTTP status codes to ErrorType
func ClassifyHTTPError(statusCode int) ErrorType {
	switch {
	case statusCode == 401 || statusCode == 403:
		return ErrorAuth
	case statusCode == 429:
		return ErrorRateLimit
	case statusCode >= 500:
		return ErrorTransient
	default:
		return ErrorFatal
	}
}
