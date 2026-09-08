// Package httpx contains the transport helpers shared by every handler:
// response envelopes, request decoding, pagination parsing and the single
// error-rendering function.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
)

// Envelope is the success shape returned by every endpoint that returns a body.
//
// Wrapping payloads in "data" costs one level of nesting but buys two things:
// list and single responses have the same top-level shape, and we can add
// sibling keys (pagination today, warnings or deprecation notices later) without
// a breaking change.
type Envelope struct {
	Data       any         `json:"data"`
	Pagination *Pagination `json:"pagination,omitempty"`
	RequestID  string      `json:"request_id,omitempty"`
}

// ErrorEnvelope is the failure shape returned by every endpoint.
type ErrorEnvelope struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"request_id,omitempty"`
}

// ErrorBody is the client-visible description of a failure. It never contains
// the internal cause.
type ErrorBody struct {
	Code    apierr.Code         `json:"code"`
	Message string              `json:"message"`
	Fields  []apierr.FieldError `json:"fields,omitempty"`
}

// WriteJSON serialises v as JSON with the given status code.
//
// The body is marshalled before the header is written so that a marshalling
// failure can still produce a coherent 500 instead of a truncated 200.
func WriteJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		logging.FromContext(ctx).Error("failed to marshal response body", slog.Any("error", err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		// Constant literal: cannot itself fail to marshal.
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"an internal error occurred"}}`))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// The client hung up mid-write. Nothing to recover, but it should be
		// visible in the logs rather than silently dropped.
		logging.FromContext(ctx).Warn("failed to write response body", slog.Any("error", err))
	}
}

// WriteData renders a single resource inside the success envelope.
func WriteData(ctx context.Context, w http.ResponseWriter, status int, data any) {
	WriteJSON(ctx, w, status, Envelope{Data: data, RequestID: RequestID(ctx)})
}

// WriteList renders a page of resources together with its pagination metadata.
// data should always be a non-nil slice so the JSON is [] rather than null.
func WriteList(ctx context.Context, w http.ResponseWriter, data any, p Pagination) {
	WriteJSON(ctx, w, http.StatusOK, Envelope{Data: data, Pagination: &p, RequestID: RequestID(ctx)})
}

// WriteNoContent completes a request that has no response body.
func WriteNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// WriteError renders err using the status and code it carries, and is the only
// function in the service that turns an error into a response.
//
// Server-side failures (5xx) are logged at error level with the internal cause
// attached; client-side failures (4xx) are logged at debug level, because a
// stream of 404s is normal traffic and should not page anyone.
func WriteError(ctx context.Context, w http.ResponseWriter, err error) {
	appErr := apierr.From(err)
	if appErr == nil {
		return
	}

	log := logging.FromContext(ctx)
	attrs := []any{
		slog.String("error_code", string(appErr.Code)),
		slog.Int("status", appErr.Status),
		slog.String("error", appErr.Error()),
	}
	if appErr.Status >= http.StatusInternalServerError {
		log.Error("request failed", attrs...)
	} else {
		log.Debug("request rejected", attrs...)
	}

	WriteJSON(ctx, w, appErr.Status, ErrorEnvelope{
		Error: ErrorBody{
			Code:    appErr.Code,
			Message: appErr.Message,
			Fields:  appErr.Fields,
		},
		RequestID: RequestID(ctx),
	})
}
