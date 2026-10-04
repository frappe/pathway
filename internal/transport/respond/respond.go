// Package respond is the one place that decides what an error looks like on the wire. Handlers and
// middleware both write through it, so a refusal reads the same whichever layer produced it.
package respond

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/phot0n/pathway/internal/domain"
)

// Error writes the shape an OpenAI client expects to parse. type is the error's class, named off
// the status as the Anthropic API names it.
func Error(w http.ResponseWriter, status int, message string) {
	TypedError(w, status, domain.AnthropicErrorType(status), message)
}

// TypedError is Error for a refusal the status alone does not name, such as maintenance.
func TypedError(w http.ResponseWriter, status int, errorType, message string) {
	Status(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": errorType},
	})
}

// ErrorFor answers in the dialect the request's surface declares — an Anthropic SDK cannot parse
// the OpenAI envelope. Everything outside the Anthropic surface keeps the OpenAI shape, which is
// what every tool that scrapes gateways assumes.
func ErrorFor(w http.ResponseWriter, r *http.Request, status int, message string) {
	TypedErrorFor(w, r, status, domain.AnthropicErrorType(status), message)
}

// TypedErrorFor is TypedError in the request surface's own dialect.
func TypedErrorFor(w http.ResponseWriter, r *http.Request, status int, errorType, message string) {
	if !anthropicSurface(r) {
		TypedError(w, status, errorType, message)
		return
	}
	Status(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType, "message": message},
	})
}

type dialectKey struct{}

// WithDialect records the surface a request arrived on. The /anthropic alias sets it as it strips
// the prefix, the only point that still knows which surface it was.
func WithDialect(ctx context.Context, dialect string) context.Context {
	return context.WithValue(ctx, dialectKey{}, dialect)
}

// Dialect is the surface a request arrived on. Root is the OpenAI surface, so unset is OpenAI.
func Dialect(ctx context.Context) string {
	if d, _ := ctx.Value(dialectKey{}).(string); d != "" {
		return d
	}
	return domain.DialectOpenAI
}

// anthropicSurface is anything under /anthropic before the alias strips it, and whatever the alias
// marked after — the data chain only ever sees the stripped form.
func anthropicSurface(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/anthropic/") || Dialect(r.Context()) == domain.DialectAnthropic
}

// Denial answers whatever the admission path refused with. Anything that is not a Denial is a bug
// in this process rather than a decision about the caller, so it is a 500 and says nothing more.
func Denial(w http.ResponseWriter, err error) {
	var denial domain.Denial
	if errors.As(err, &denial) {
		retryAfter(w, denial)
		Error(w, denial.Status, denial.Reason)
		return
	}
	Error(w, http.StatusInternalServerError, "gateway error")
}

// retryAfter tells the client when a refusal that knows its own end will lift.
func retryAfter(w http.ResponseWriter, denial domain.Denial) {
	if denial.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(denial.RetryAfter))
	}
}

// DenialFor is Denial in the request surface's own dialect.
func DenialFor(w http.ResponseWriter, r *http.Request, err error) {
	var denial domain.Denial
	if errors.As(err, &denial) {
		retryAfter(w, denial)
		ErrorFor(w, r, denial.Status, denial.Reason)
		return
	}
	ErrorFor(w, r, http.StatusInternalServerError, "gateway error")
}

func JSON(w http.ResponseWriter, v any) { Status(w, http.StatusOK, v) }

func Status(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
