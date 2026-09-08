package apierr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConstructorsUseTheExpectedStatusCodes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err        *Error
		wantStatus int
		wantCode   Code
	}{
		"validation":   {Validation(), http.StatusUnprocessableEntity, CodeValidation},
		"bad request":  {BadRequest("nope"), http.StatusBadRequest, CodeBadRequest},
		"unauthorized": {Unauthorized("nope"), http.StatusUnauthorized, CodeUnauthorized},
		"forbidden":    {Forbidden("nope"), http.StatusForbidden, CodeForbidden},
		"not found":    {NotFound("post"), http.StatusNotFound, CodeNotFound},
		"conflict":     {Conflict("nope"), http.StatusConflict, CodeConflict},
		"rate limited": {RateLimited("nope"), http.StatusTooManyRequests, CodeRateLimited},
		"timeout":      {Timeout("nope"), http.StatusGatewayTimeout, CodeTimeout},
		"unavailable":  {Unavailable("nope"), http.StatusServiceUnavailable, CodeUnavailable},
		"internal":     {Internal(errors.New("boom")), http.StatusInternalServerError, CodeInternal},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.wantStatus, tc.err.Status)
			assert.Equal(t, tc.wantCode, tc.err.Code)
			assert.NotEmpty(t, tc.err.Message)
		})
	}
}

func TestNotFoundNamesTheResource(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "comment not found", NotFound("comment").Message)
}

// TestInternalHidesCauseFromClientMessage is the central privacy property of
// this package: the driver error must reach the log and never the response.
func TestInternalHidesCauseFromClientMessage(t *testing.T) {
	t.Parallel()

	secret := errors.New(`pq: password authentication failed for user "blog" at 10.0.0.7`)

	err := Internal(fmt.Errorf("select user: %w", secret))

	assert.Equal(t, "an internal error occurred", err.Message)
	assert.NotContains(t, err.Message, "password")
	// The cause is still reachable for logging and for errors.Is.
	assert.Contains(t, err.Error(), "password authentication failed")
	assert.True(t, errors.Is(err, secret))
}

func TestWithCauseDoesNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	// Package-level errors are shared. If WithCause mutated in place, one
	// request's cause would leak into another's error.
	shared := Conflict("slug taken")

	withCause := shared.WithCause(errors.New("unique violation"))

	assert.Nil(t, shared.Unwrap(), "the original must be untouched")
	assert.NotNil(t, withCause.Unwrap())
	assert.NotSame(t, shared, withCause)
}

func TestWithMessageDoesNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	shared := Forbidden("original")

	changed := shared.WithMessage("replacement")

	assert.Equal(t, "original", shared.Message)
	assert.Equal(t, "replacement", changed.Message)
	assert.Equal(t, shared.Status, changed.Status)
}

func TestFromPassesThroughExistingError(t *testing.T) {
	t.Parallel()

	original := NotFound("post")

	assert.Same(t, original, From(original))
	assert.Same(t, original, From(fmt.Errorf("wrapped: %w", original)),
		"From must find an *Error anywhere in the chain")
}

func TestFromClassifiesUnknownErrorAsInternal(t *testing.T) {
	t.Parallel()

	converted := From(errors.New("something nobody classified"))

	require.NotNil(t, converted)
	assert.Equal(t, CodeInternal, converted.Code, "an unclassified error must default to 500, never to 400")
}

func TestFromReturnsNilForNilError(t *testing.T) {
	t.Parallel()
	assert.Nil(t, From(nil))
}

// TestContextErrorsAreReclassified guards a real trap: repositories wrap every
// driver failure with Internal, and a query aborted by the request deadline must
// not be reported as a server fault.
func TestContextErrorsAreReclassified(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cause      error
		wantStatus int
		wantCode   Code
	}{
		"deadline exceeded": {context.DeadlineExceeded, http.StatusGatewayTimeout, CodeTimeout},
		"canceled":          {context.Canceled, StatusClientClosedRequest, CodeBadRequest},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			viaInternal := Internal(fmt.Errorf("query posts: %w", tc.cause))
			assert.Equal(t, tc.wantStatus, viaInternal.Status)
			assert.Equal(t, tc.wantCode, viaInternal.Code)

			viaFrom := From(fmt.Errorf("query posts: %w", tc.cause))
			assert.Equal(t, tc.wantStatus, viaFrom.Status)
		})
	}
}

func TestIsCode(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("service layer: %w", Conflict("duplicate"))

	assert.True(t, IsCode(err, CodeConflict))
	assert.False(t, IsCode(err, CodeNotFound))
	assert.False(t, IsCode(errors.New("plain"), CodeConflict))
	assert.False(t, IsCode(nil, CodeConflict))
}

func TestValidationCarriesFieldErrors(t *testing.T) {
	t.Parallel()

	err := Validation(
		FieldError{Field: "email", Message: "must be a valid email address"},
		FieldError{Field: "password", Message: "must be at least 8 characters long"},
	)

	require.Len(t, err.Fields, 2)
	assert.Equal(t, "email", err.Fields[0].Field)

	// The JSON names are part of the API contract.
	encoded, marshalErr := json.Marshal(err.Fields[0])
	require.NoError(t, marshalErr)
	assert.JSONEq(t, `{"field":"email","message":"must be a valid email address"}`, string(encoded))
}

func TestErrorStringIncludesCauseForLogs(t *testing.T) {
	t.Parallel()

	withoutCause := NotFound("post")
	assert.Equal(t, "not_found: post not found", withoutCause.Error())

	withCause := withoutCause.WithCause(errors.New("no rows in result set"))
	assert.Contains(t, withCause.Error(), "no rows in result set")
}
