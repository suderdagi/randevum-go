// Package httpx has the small helpers every handler uses: JSON in and out,
// a typed error that knows its HTTP status, and a consistent error body.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Error is an error the client is allowed to see.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func BadRequest(msg string) *Error { return &Error{http.StatusBadRequest, "BAD_REQUEST", msg, ""} }
func Invalid(field, msg string) *Error {
	return &Error{http.StatusBadRequest, "VALIDATION_ERROR", msg, field}
}
func Unauthorized(msg string) *Error { return &Error{http.StatusUnauthorized, "UNAUTHORIZED", msg, ""} }
func Forbidden(msg string) *Error    { return &Error{http.StatusForbidden, "FORBIDDEN", msg, ""} }
func NotFound(what string) *Error {
	return &Error{http.StatusNotFound, "NOT_FOUND", what + " not found", ""}
}
func Conflict(code, msg string) *Error { return &Error{http.StatusConflict, code, msg, ""} }

// Handler is an http handler that can return an error instead of writing it itself.
type Handler func(w http.ResponseWriter, r *http.Request) error

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}

// WriteError turns any error into a JSON response. Database errors that mean
// "the client did something invalid" are mapped to 4xx codes; everything else
// is logged and hidden behind a generic 500.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &appErr):
	case errors.Is(err, pgx.ErrNoRows):
		appErr = NotFound("Resource")
	case errors.As(err, &pgErr) && pgErr.Code == "23P01": // exclusion_violation
		appErr = Conflict("SLOT_TAKEN", "This time slot is no longer available")
	case errors.As(err, &pgErr) && pgErr.Code == "23505": // unique_violation
		appErr = Conflict("ALREADY_EXISTS", "A record with these details already exists")
	default:
		slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		appErr = &Error{http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong", ""}
	}
	JSON(w, appErr.Status, map[string]any{"error": appErr})
}

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Decode reads a JSON body into dst, rejecting unknown fields and bodies over 1 MB.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return BadRequest(fmt.Sprintf("Invalid JSON body: %v", err))
	}
	return nil
}

// PathID parses a numeric path parameter such as {id}.
func PathID(r *http.Request, name string) (int, error) {
	id, err := strconv.Atoi(r.PathValue(name))
	if err != nil || id <= 0 {
		return 0, Invalid(name, name+" must be a positive number")
	}
	return id, nil
}

// Page reads ?page= and ?limit= with defaults (1, 20) and a maximum limit of 100.
func Page(r *http.Request) (limit, offset int, err error) {
	page, limit := 1, 20
	if v := r.URL.Query().Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			return 0, 0, Invalid("page", "page must be a positive number")
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			return 0, 0, Invalid("limit", "limit must be between 1 and 100")
		}
	}
	return limit, (page - 1) * limit, nil
}
