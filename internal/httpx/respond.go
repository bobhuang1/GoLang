// Package httpx provides JSON response helpers, request decoding and the
// shared HTTP middleware (request id, recover, logging, auth, idempotency).
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// ctxKey is a private type for context values.
type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxUser
)

// ContextUser carries the authenticated caller into handlers.
type ContextUser struct {
	CustomerID int64
	Email      string
	Role       string
}

// UserFrom returns the authenticated user or nil.
func UserFrom(ctx context.Context) *ContextUser {
	u, _ := ctx.Value(ctxUser).(*ContextUser)
	return u
}

// WithUser stores the authenticated user in the request context.
func WithUser(ctx context.Context, u *ContextUser) context.Context {
	return context.WithValue(ctx, ctxUser, u)
}

// RequestID returns the current request id, generating one if absent.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// Envelope is the standard error shape returned on failure.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError carries a machine-friendly code and a human message.
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is a typed HTTP error raised by the domain layers.
type APIError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause.
func (e *APIError) Unwrap() error { return e.Err }

// Common API error constructors.
func BadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}
func NotFound(msg string) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: "not_found", Message: msg}
}
func Conflict(msg string) *APIError {
	return &APIError{Status: http.StatusConflict, Code: "conflict", Message: msg}
}
func Unauthorized(msg string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: msg}
}
func Forbidden(msg string) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
}
func Internal(msg string, err error) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Code: "internal", Message: msg, Err: err}
}

// Wrap wraps a plain error as a 500. A nil input stays nil so callers can
// `return httpx.Wrap(e)` (as internal/email does after a successful Exec)
// without manufacturing a synthetic error on the happy path.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	return &APIError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error", Err: err}
}

// WriteJSON writes a JSON body.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteOK writes a 200 JSON body.
func WriteOK(w http.ResponseWriter, payload any) { WriteJSON(w, http.StatusOK, payload) }

// WriteCreated writes a 201 JSON body.
func WriteCreated(w http.ResponseWriter, payload any) { WriteJSON(w, http.StatusCreated, payload) }

// WriteNoContent writes a 204.
func WriteNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// WriteError maps a value to the error envelope. It unwraps *APIError stages;
// anything else becomes a generic 500.
func WriteError(w http.ResponseWriter, err error) {
	if err == nil {
		WriteNoContent(w)
		return
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		WriteJSON(w, apiErr.Status, Envelope{Error: EnvelopeError{Code: apiErr.Code, Message: apiErr.Message}})
		return
	}
	WriteJSON(w, http.StatusInternalServerError, Envelope{
		Error: EnvelopeError{Code: "internal", Message: "internal error"},
	})
}

// DecodeJSON reads a request body (bounded) into dst.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	body := io.LimitReader(r.Body, 1<<20)
	defer r.Body.Close()
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		WriteError(w, BadRequest("malformed JSON body"))
		return err
	}
	return nil
}

// ParsePathID parses a chi URL parameter as an int64.
func ParsePathID(r *http.Request, name string) (int64, error) {
	raw := chi.URLParam(r, name)
	if raw == "" {
		return 0, BadRequest("missing path parameter " + name)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, BadRequest("invalid path parameter " + name)
	}
	return id, nil
}

// ParsePathUUID returns the raw UUID path parameter (validated as non-empty).
func ParsePathUUID(r *http.Request, name string) (string, error) {
	raw := chi.URLParam(r, name)
	if raw == "" {
		return "", BadRequest("missing path parameter " + name)
	}
	return raw, nil
}
