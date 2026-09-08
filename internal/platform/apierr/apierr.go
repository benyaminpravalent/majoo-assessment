// Package apierr defines the single error type that crosses layer boundaries in
// this service.
//
// The design goal is that a handler never decides an HTTP status code. A
// service or repository states what went wrong in domain terms, and exactly one
// place — httpx.WriteError — turns that into a response. This keeps status
// codes consistent across the whole API and makes it structurally hard to leak
// an internal message: the client-visible Message is a separate field from the
// wrapped cause, and only the former is ever serialised.
package apierr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Clients should branch on
// Code, never on the human-readable message.
type Code string

const (
	CodeValidation      Code = "validation_failed"
	CodeBadRequest      Code = "bad_request"
	CodeUnauthorized    Code = "unauthorized"
	CodeForbidden       Code = "forbidden"
	CodeNotFound        Code = "not_found"
	CodeConflict        Code = "conflict"
	CodePayloadTooLarge Code = "payload_too_large"
	CodeUnsupportedType Code = "unsupported_media_type"
	CodeRateLimited     Code = "rate_limited"
	CodeTimeout         Code = "request_timeout"
	CodeInternal        Code = "internal_error"
	CodeUnavailable     Code = "service_unavailable"
)

// FieldError points at one invalid input field. Field uses the JSON name the
// client sent, not the Go struct field, so the message is actionable.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is an application error carrying everything the transport layer needs.
//
// cause is unexported and never serialised: it holds the driver error, the
// failing SQL condition or the wrapped library error, which is exactly the sort
// of detail that must reach the logs and never reach the client.
type Error struct {
	Code    Code
	Status  int
	Message string
	Fields  []FieldError

	cause error
}

// Error implements the error interface. The cause is included here because this
// string goes to logs, not to clients.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying error. It returns a copy so that shared
// package-level errors cannot be mutated by a caller.
func (e *Error) WithCause(err error) *Error {
	cp := *e
	cp.cause = err
	return &cp
}

// WithMessage overrides the client-visible message, returning a copy.
func (e *Error) WithMessage(msg string) *Error {
	cp := *e
	cp.Message = msg
	return &cp
}

// New builds an Error with an explicit code, status and client-safe message.
func New(code Code, status int, msg string) *Error {
	return &Error{Code: code, Status: status, Message: msg}
}

// Validation reports one or more invalid request fields.
func Validation(fields ...FieldError) *Error {
	return &Error{
		Code:    CodeValidation,
		Status:  http.StatusUnprocessableEntity,
		Message: "one or more fields are invalid",
		Fields:  fields,
	}
}

// BadRequest reports a malformed request that never reached field validation,
// such as a body that is not valid JSON.
func BadRequest(msg string) *Error { return New(CodeBadRequest, http.StatusBadRequest, msg) }

// Unauthorized reports missing or unusable authentication credentials.
func Unauthorized(msg string) *Error { return New(CodeUnauthorized, http.StatusUnauthorized, msg) }

// Forbidden reports an authenticated caller acting outside their permissions.
func Forbidden(msg string) *Error { return New(CodeForbidden, http.StatusForbidden, msg) }

// NotFound reports a missing resource. The resource name is interpolated so the
// message stays specific without the caller having to write a sentence.
func NotFound(resource string) *Error {
	return New(CodeNotFound, http.StatusNotFound, resource+" not found")
}

// Conflict reports a request that collides with existing state, such as a
// duplicate email address.
func Conflict(msg string) *Error { return New(CodeConflict, http.StatusConflict, msg) }

// RateLimited reports that the caller exceeded their request budget.
func RateLimited(msg string) *Error { return New(CodeRateLimited, http.StatusTooManyRequests, msg) }

// Timeout reports that the request exceeded its server-side deadline.
func Timeout(msg string) *Error { return New(CodeTimeout, http.StatusGatewayTimeout, msg) }

// Unavailable reports that a dependency the request needs is not usable.
func Unavailable(msg string) *Error {
	return New(CodeUnavailable, http.StatusServiceUnavailable, msg)
}

// Internal wraps an unexpected failure. The client-visible message is
// deliberately generic and constant; the real cause travels in cause and is
// written to the request log by httpx.WriteError.
//
// A context failure is reclassified rather than reported as a fault. Repository
// code calls Internal on any error the driver returns, and a query aborted by
// the request deadline is not a server bug — it must surface as 504, not 500,
// or every client timeout inflates the service's error rate.
func Internal(cause error) *Error {
	if classified := classifyContextError(cause); classified != nil {
		return classified
	}
	return (&Error{
		Code:    CodeInternal,
		Status:  http.StatusInternalServerError,
		Message: "an internal error occurred",
	}).WithCause(cause)
}

// classifyContextError recognises deadline and cancellation failures, which can
// surface from any layer that respects a context.
func classifyContextError(err error) *Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout("the request took too long to process").WithCause(err)
	case errors.Is(err, context.Canceled):
		// The client hung up. Nothing will read this response, but a distinct
		// status is still recorded so the access log and dashboards separate
		// abandoned requests from real faults.
		return New(CodeBadRequest, StatusClientClosedRequest, "client closed the request").WithCause(err)
	default:
		return nil
	}
}

// From converts an arbitrary error into an *Error. Anything that is not already
// an *Error is treated as an internal failure, which is the safe default: an
// error nobody classified must not be described to the client.
func From(err error) *Error {
	if err == nil {
		return nil
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	return Internal(err)
}

// StatusClientClosedRequest is nginx's non-standard 499. There is no IANA code
// for "the client went away", and using 500 would make abandoned requests
// indistinguishable from real faults on a dashboard.
const StatusClientClosedRequest = 499

// IsCode reports whether err is (or wraps) an *Error with the given code.
func IsCode(err error, code Code) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}
