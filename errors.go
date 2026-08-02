package emailclient

import (
	"errors"
	"fmt"
)

// Sentinel errors let callers switch on the failure category with errors.Is
// without pattern-matching on string codes. They map to the stable error codes
// the service returns (see APIError.Is).
var (
	// ErrUnauthorized is returned for a missing or invalid API key (HTTP 401).
	ErrUnauthorized = errors.New("emailclient: unauthorized")
	// ErrRateLimited is returned when the per-client rate limit is exceeded (HTTP 429).
	ErrRateLimited = errors.New("emailclient: rate limited")
	// ErrInvalidRequest is returned for a malformed or invalid request (HTTP 400/413).
	ErrInvalidRequest = errors.New("emailclient: invalid request")
	// ErrInvalidSenderDomain is returned when the client may not send from the
	// given "from" domain (HTTP 400).
	ErrInvalidSenderDomain = errors.New("emailclient: invalid sender domain")
	// ErrProviderFailure is returned when the downstream email provider fails (HTTP 502).
	ErrProviderFailure = errors.New("emailclient: email provider failure")
	// ErrInternal is returned for an unexpected server error (HTTP 500).
	ErrInternal = errors.New("emailclient: internal error")
)

// APIError is a structured error returned by the email service. It carries the
// HTTP status, the stable machine-readable code, and a human-readable message.
// Use errors.Is against the sentinel errors above to branch on the category, or
// inspect the fields directly.
type APIError struct {
	// StatusCode is the HTTP status returned by the service.
	StatusCode int
	// Code is the stable, machine-readable identifier (e.g. "rate_limited").
	Code string
	// Message is the human-readable explanation from the service.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("emailclient: %s (code=%s, status=%d)", e.Message, e.Code, e.StatusCode)
}

// Is maps the service's stable error codes onto the sentinel errors so callers
// can write errors.Is(err, emailclient.ErrRateLimited).
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == "unauthorized"
	case ErrRateLimited:
		return e.Code == "rate_limited"
	case ErrInvalidRequest:
		return e.Code == "invalid_request"
	case ErrInvalidSenderDomain:
		return e.Code == "invalid_sender_domain"
	case ErrProviderFailure:
		return e.Code == "ses_failure"
	case ErrInternal:
		return e.Code == "internal_error"
	default:
		return false
	}
}
