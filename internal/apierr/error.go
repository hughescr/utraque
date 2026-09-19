// Package apierr is utraque's canonical error type. It maps an error onto an
// Anthropic error envelope and the matching HTTP status, so every failure the
// proxy produces looks to the client exactly like an upstream failure.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/hughescr/utraque/internal/anthropic/schema"
)

// ErrorType is an Anthropic error type string.
type ErrorType string

// The Anthropic error taxonomy.
const (
	TypeInvalidRequest  ErrorType = "invalid_request_error"
	TypeAuthentication  ErrorType = "authentication_error"
	TypePermission      ErrorType = "permission_error"
	TypeNotFound        ErrorType = "not_found_error"
	TypeRequestTooLarge ErrorType = "request_too_large"
	TypeRateLimit       ErrorType = "rate_limit_error"
	TypeAPI             ErrorType = "api_error"
	TypeOverloaded      ErrorType = "overloaded_error"
	TypeTimeout         ErrorType = "timeout_error"
)

// Error is a client-renderable failure.
type Error struct {
	Type    ErrorType
	Message string
	Status  int   // 0 means "derive from Type"
	Err     error // cause; never serialized
}

var _ error = (*Error)(nil)

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Type, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
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
	return StatusFor(e.Type)
}

// Envelope renders the Anthropic error envelope.
func (e *Error) Envelope() aschema.ErrorEvent {
	if e == nil {
		return aschema.NewErrorEvent(string(TypeAPI), "internal error")
	}
	errType := e.Type
	if errType == "" {
		errType = TypeAPI
	}
	return aschema.NewErrorEvent(string(errType), e.Message)
}

// Render writes the envelope. A status <= 0 derives one from Type.
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
func New(errType ErrorType, format string, args ...any) *Error {
	return &Error{Type: errType, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an Error carrying a cause.
func Wrap(err error, errType ErrorType, format string, args ...any) *Error {
	return &Error{Type: errType, Message: fmt.Sprintf(format, args...), Err: err}
}

// WithStatus builds an Error pinned to an explicit HTTP status.
func WithStatus(status int, errType ErrorType, format string, args ...any) *Error {
	return &Error{Type: errType, Message: fmt.Sprintf(format, args...), Status: status}
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
func StatusFor(errType ErrorType) int {
	switch errType {
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
func TypeForStatus(status int) ErrorType {
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
