package outis

import (
	"errors"
	"fmt"
	"time"
)

// ErrTimeoutRequired is returned when a wait is given no timeout.
var ErrTimeoutRequired = errors.New("outis: a wait needs a timeout greater than zero")

// ErrTimeoutTooLong is returned when a wait asks for more than [MaxWaitTimeout].
var ErrTimeoutTooLong = errors.New("outis: wait timeout is over the cap")

// APIError is a refusal from the Outis API.
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Code is the API's machine readable reason from the body's "kind" field,
	// such as "idempotency_conflict" or "not_found". Empty when the API didn't
	// send one.
	Code string
	// Message is the API's explanation, in words.
	Message string
	// RetryAfter is the server's Retry-After, when it sent one.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("outis: HTTP %d", e.StatusCode)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// WaitTimeoutError means the wait ran out while the request was still live.
// The request carries on in Outis; resume later with its id.
type WaitTimeoutError struct {
	RequestID string
	// Request is the last state read, or nil if none was read.
	Request *Request
	Timeout time.Duration
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf("outis: request %s still pending after %s", e.RequestID, e.Timeout)
}

// NotAuthorizedError means the request isn't authorized: it was denied,
// expired or aborted, or it's still pending (Outcome is empty).
type NotAuthorizedError struct {
	RequestID string
	Outcome   Outcome
	Request   *Request
}

func (e *NotAuthorizedError) Error() string {
	if e.Outcome == "" {
		return fmt.Sprintf("outis: request %s is still pending", e.RequestID)
	}
	return fmt.Sprintf("outis: request %s was %s", e.RequestID, e.Outcome)
}

// OperationMismatchError means a request was authorized for a different
// operation than the one about to run.
type OperationMismatchError struct {
	RequestID string
	// Expected is the hash of the operation the caller is about to run.
	Expected string
	// Actual is the hash of the operation the operators authorized.
	Actual  string
	Request *Request
}

func (e *OperationMismatchError) Error() string {
	return fmt.Sprintf("outis: request %s authorized %s, not %s", e.RequestID, e.Actual, e.Expected)
}

// WebhookVerificationError means a webhook delivery failed verification and
// must not be trusted.
type WebhookVerificationError struct {
	Reason string
}

func (e *WebhookVerificationError) Error() string {
	return "outis: webhook verification failed: " + e.Reason
}
