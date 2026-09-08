// Package validation adapts go-playground/validator to this service's error
// contract.
//
// The library is used only for the mechanical part — walking struct tags. The
// translation from a failed rule to a client-facing message lives here, so the
// API returns "must be at least 8 characters long" rather than leaking the
// library's internal representation, and so the field name in the response is
// the JSON name the client actually sent.
package validation

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/go-playground/validator/v10"
)

// Validator wraps a configured validator instance. One instance is built at
// start-up and shared: it caches struct reflection internally and is safe for
// concurrent use.
type Validator struct {
	v *validator.Validate
}

// New returns a Validator that reports errors using JSON field names.
func New() *Validator {
	v := validator.New(validator.WithRequiredStructEnabled())

	// Report the JSON name, so a client that sent "display_name" is told about
	// "display_name" and not about "DisplayName".
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})

	// "username" is registered rather than expressed as a raw regex tag on each
	// DTO: the rule appears in three request types and must stay identical to the
	// CHECK constraint on users.username, so it gets one definition.
	// RegisterValidation only fails on an empty tag name or nil function.
	if err := v.RegisterValidation("username", func(fl validator.FieldLevel) bool {
		return usernamePattern.MatchString(fl.Field().String())
	}); err != nil {
		panic(fmt.Sprintf("validation: registering username rule: %v", err))
	}

	return &Validator{v: v}
}

// usernamePattern mirrors the users_username_format CHECK constraint in
// migrations/0001_init.up.sql. Keeping both is intentional: the database is the
// last line of defence, the application gives the better error message.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,30}$`)

// Struct validates s and returns an *apierr.Error with one FieldError per
// failed rule, or nil when s is valid.
func (val *Validator) Struct(s any) error {
	err := val.v.Struct(s)
	if err == nil {
		return nil
	}

	// An InvalidValidationError means we passed something unvalidatable (a nil
	// pointer, a non-struct). That is our bug, not the client's.
	var invalid *validator.InvalidValidationError
	if errors.As(err, &invalid) {
		return apierr.Internal(err)
	}

	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return apierr.Internal(err)
	}

	fields := make([]apierr.FieldError, 0, len(verrs))
	for _, fe := range verrs {
		fields = append(fields, apierr.FieldError{
			Field:   fe.Field(),
			Message: message(fe),
		})
	}
	return apierr.Validation(fields...).WithCause(err)
}

// message renders one failed rule as a sentence a client can act on.
func message(fe validator.FieldError) string {
	param := fe.Param()

	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "min":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("must be at least %s characters long", param)
		}
		return fmt.Sprintf("must be at least %s", param)
	case "max":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("must be at most %s characters long", param)
		}
		return fmt.Sprintf("must be at most %s", param)
	case "len":
		return fmt.Sprintf("must be exactly %s characters long", param)
	case "oneof":
		return fmt.Sprintf("must be one of: %s", strings.ReplaceAll(param, " ", ", "))
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "alphanum":
		return "must contain only letters and digits"
	case "url":
		return "must be a valid URL"
	case "gte":
		return fmt.Sprintf("must be greater than or equal to %s", param)
	case "lte":
		return fmt.Sprintf("must be less than or equal to %s", param)
	case "excludesall":
		return fmt.Sprintf("must not contain any of the characters: %s", param)
	case "username":
		return "must be 3-30 characters of letters, digits, underscore or hyphen"
	default:
		// Unknown rule: still return something specific enough to debug, without
		// pretending to a friendlier message we have not written.
		if param != "" {
			return fmt.Sprintf("failed the %q rule (%s)", fe.Tag(), param)
		}
		return fmt.Sprintf("failed the %q rule", fe.Tag())
	}
}
