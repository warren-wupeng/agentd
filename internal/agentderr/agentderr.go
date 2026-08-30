// Package agentderr is the single error type crossing all layers (G5).
// Every error states a remediation, not just a failure — the primary
// consumer of an error message is an agent deciding what to do next.
package agentderr

import "net/http"

// Code is a stable, machine-readable error identifier. Clients branch on
// Code, never on Message text.
type Code string

const (
	CodeInvalidInput      Code = "INVALID_INPUT"
	CodeNotFound          Code = "NOT_FOUND"
	CodeConflict          Code = "CONFLICT"
	CodeInvalidTransition Code = "INVALID_TRANSITION"
	CodeImmutable         Code = "IMMUTABLE"
	CodeInternal          Code = "INTERNAL"
)

// Error carries a failure plus what to do about it.
type Error struct {
	Code        Code   `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	cause       error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// New builds an Error with an explicit message.
func New(code Code, message, remediation string) *Error {
	return &Error{Code: code, Message: message, Remediation: remediation}
}

// Wrap keeps the underlying cause for logs while presenting a clean message.
func Wrap(code Code, cause error, message, remediation string) *Error {
	return &Error{Code: code, Message: message, Remediation: remediation, cause: cause}
}

func InvalidInput(message, remediation string) *Error {
	return New(CodeInvalidInput, message, remediation)
}

func NotFound(message, remediation string) *Error {
	return New(CodeNotFound, message, remediation)
}

func Conflict(message, remediation string) *Error {
	return New(CodeConflict, message, remediation)
}

func InvalidTransition(message, remediation string) *Error {
	return New(CodeInvalidTransition, message, remediation)
}

// Internal is for unexpected failures; the remediation never leaks internals.
func Internal(cause error) *Error {
	return Wrap(CodeInternal, cause, "unexpected server error",
		"retry the request; if it persists, check server logs")
}

// HTTPStatus maps a Code to its HTTP status. The API layer is the only
// caller; codes are transport-neutral everywhere else.
func HTTPStatus(c Code) int {
	switch c {
	case CodeInvalidInput:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeInvalidTransition, CodeImmutable:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
