// Package apierr is utraque's canonical error type. It maps an error onto an
// Anthropic error envelope and the matching HTTP status, so every failure the
// proxy produces looks to the client exactly like an upstream failure.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hughescr/utraque/internal/anthropic/schema"
)

// Type is an Anthropic error type string.
type Type string

// The Anthropic error taxonomy.
const (
	TypeInvalidRequest  Type = "invalid_request_error"
	TypeAuthentication  Type = "authentication_error"
	TypePermission      Type = "permission_error"
	TypeNotFound        Type = "not_found_error"
	TypeRequestTooLarge Type = "request_too_large"
	TypeRateLimit       Type = "rate_limit_error"
	TypeAPI             Type = "api_error"
	TypeOverloaded      Type = "overloaded_error"
	TypeTimeout         Type = "timeout_error"
)

// Error is a client-renderable failure.
type Error struct {
	Kind    Type
	Message string
	Status  int   // 0 means "derive from Kind"
	Err     error // cause; never serialized
}

var _ error = (*Error)(nil)

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// HTTPStatus is the status to send, explicit or derived.
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	if e.Status > 0 {
		return e.Status
	}
	return StatusFor(e.Kind)
}

// Envelope renders the Anthropic error envelope.
func (e *Error) Envelope() schema.ErrorEvent {
	if e == nil {
		return schema.NewErrorEvent(string(TypeAPI), "internal error")
	}
	kind := e.Kind
	if kind == "" {
		kind = TypeAPI
	}
	return schema.NewErrorEvent(string(kind), e.Message)
}

// Render writes the envelope. A status <= 0 derives one from Kind.
func (e *Error) Render(w http.ResponseWriter, status int) error {
	if status <= 0 {
		status = e.HTTPStatus()
	}
	body, err := json.Marshal(e.Envelope())
	if err != nil {
		return err
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// New builds an Error.
func New(kind Type, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an Error carrying a cause.
func Wrap(err error, kind Type, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
}

// WithStatus builds an Error pinned to an explicit HTTP status.
func WithStatus(status int, kind Type, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Status: status}
}

// InvalidRequest builds a 400.
func InvalidRequest(format string, args ...any) *Error {
	return New(TypeInvalidRequest, format, args...)
}

// Authentication builds a 401.
func Authentication(format string, args ...any) *Error {
	return New(TypeAuthentication, format, args...)
}

// Permission builds a 403.
func Permission(format string, args ...any) *Error {
	return New(TypePermission, format, args...)
}

// NotFound builds a 404.
func NotFound(format string, args ...any) *Error {
	return New(TypeNotFound, format, args...)
}

// UnknownModel is a router-facing convenience over NotFound: a model name
// Resolve couldn't place in any backend, rendered as a 404 listing the
// known route families so the caller can see what would have worked.
func UnknownModel(model string, families []string) *Error {
	return NotFound("model %q not recognised; known route families: %s", model, strings.Join(families, ", "))
}

// RequestTooLarge builds a 413.
func RequestTooLarge(format string, args ...any) *Error {
	return New(TypeRequestTooLarge, format, args...)
}

// RateLimit builds a 429.
func RateLimit(format string, args ...any) *Error {
	return New(TypeRateLimit, format, args...)
}

// API builds a 500.
func API(format string, args ...any) *Error {
	return New(TypeAPI, format, args...)
}

// Overloaded builds a 529.
func Overloaded(format string, args ...any) *Error {
	return New(TypeOverloaded, format, args...)
}

// Timeout builds a 504.
func Timeout(format string, args ...any) *Error {
	return New(TypeTimeout, format, args...)
}

// StatusFor maps an error type onto its HTTP status.
func StatusFor(kind Type) int {
	switch kind {
	case TypeInvalidRequest:
		return http.StatusBadRequest
	case TypeAuthentication:
		return http.StatusUnauthorized
	case TypePermission:
		return http.StatusForbidden
	case TypeNotFound:
		return http.StatusNotFound
	case TypeRequestTooLarge:
		return http.StatusRequestEntityTooLarge
	case TypeRateLimit:
		return http.StatusTooManyRequests
	case TypeTimeout:
		return http.StatusGatewayTimeout
	case TypeOverloaded:
		return 529
	case TypeAPI:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// TypeForStatus maps an HTTP status back onto an error type.
func TypeForStatus(status int) Type {
	switch status {
	case http.StatusBadRequest:
		return TypeInvalidRequest
	case http.StatusUnauthorized:
		return TypeAuthentication
	case http.StatusForbidden:
		return TypePermission
	case http.StatusNotFound:
		return TypeNotFound
	case http.StatusRequestEntityTooLarge:
		return TypeRequestTooLarge
	case http.StatusTooManyRequests:
		return TypeRateLimit
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return TypeTimeout
	case 529, http.StatusServiceUnavailable:
		return TypeOverloaded
	default:
		return TypeAPI
	}
}

// From coerces any error into an *Error, defaulting to api_error/500.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return Wrap(err, TypeAPI, "%s", err.Error())
}

// Write renders any error as an Anthropic error envelope.
func Write(w http.ResponseWriter, err error) error {
	ae := From(err)
	if ae == nil {
		ae = API("internal error")
	}
	return ae.Render(w, 0)
}
