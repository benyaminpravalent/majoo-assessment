package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
)

// DecodeJSON reads exactly one JSON object from the request body into dst.
//
// It is stricter than a bare json.Decoder in three ways that all catch real
// client bugs early:
//
//   - the Content-Type must be application/json, so a form post does not
//     silently decode into a zero-valued struct;
//   - unknown fields are rejected, so a typo in a field name fails loudly
//     instead of being ignored;
//   - trailing content after the first value is rejected, so two concatenated
//     objects cannot be half-applied.
//
// The body is expected to already be wrapped in http.MaxBytesReader by the
// BodyLimit middleware; DecodeJSON recognises the resulting error and maps it
// to 413 rather than a generic 400.
func DecodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/json" {
			return apierr.New(apierr.CodeUnsupportedType, http.StatusUnsupportedMediaType,
				"Content-Type must be application/json")
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second successful decode means the client sent more than one value.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apierr.BadRequest("request body must contain exactly one JSON object")
	}
	return nil
}

// decodeError translates encoding/json's errors into messages that tell the
// client what to fix, without echoing back arbitrary body content.
func decodeError(err error) error {
	var (
		syntaxErr      *json.SyntaxError
		unmarshalType  *json.UnmarshalTypeError
		maxBytesErr    *http.MaxBytesError
		invalidUnmarsh *json.InvalidUnmarshalError
	)

	switch {
	case errors.As(err, &maxBytesErr):
		return apierr.New(apierr.CodePayloadTooLarge, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body must not exceed %d bytes", maxBytesErr.Limit)).WithCause(err)

	case errors.As(err, &syntaxErr):
		return apierr.BadRequest(fmt.Sprintf("malformed JSON at byte offset %d", syntaxErr.Offset)).WithCause(err)

	case errors.Is(err, io.ErrUnexpectedEOF):
		return apierr.BadRequest("malformed JSON: unexpected end of input").WithCause(err)

	case errors.As(err, &unmarshalType):
		// Field is empty for a type mismatch at the top level.
		if unmarshalType.Field != "" {
			return apierr.Validation(apierr.FieldError{
				Field:   unmarshalType.Field,
				Message: fmt.Sprintf("must be of type %s", unmarshalType.Type.String()),
			}).WithCause(err)
		}
		return apierr.BadRequest("request body has the wrong JSON type").WithCause(err)

	case errors.Is(err, io.EOF):
		return apierr.BadRequest("request body must not be empty").WithCause(err)

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apierr.Validation(apierr.FieldError{
			Field:   field,
			Message: "unknown field",
		}).WithCause(err)

	case errors.As(err, &invalidUnmarsh):
		// A nil or non-pointer destination is a programming error, not a client
		// error; surface it as 500 so it shows up in alerting.
		return apierr.Internal(err)

	default:
		return apierr.BadRequest("request body could not be decoded").WithCause(err)
	}
}
