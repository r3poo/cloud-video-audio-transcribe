package openrouter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response from the API.
type APIError struct {
	StatusCode int
	// Code is the provider's own error code when one was included.
	Code int
	// Message is the human-readable explanation.
	Message string
	// Body keeps a short excerpt of the raw response for diagnostics.
	Body string
	// RetryAfter is the server's requested cool-off, when it sent one.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, http.StatusText(e.StatusCode), msg)
	}
	return msg
}

// Retryable reports whether repeating the same request could succeed.
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.StatusCode >= 500
}

// RetryDelay is the wait before the next attempt: the server's Retry-After
// when present, otherwise exponential backoff capped at 30 seconds.
func (e *APIError) RetryDelay(attempt int) time.Duration {
	if e.RetryAfter > 0 {
		if e.RetryAfter > time.Minute {
			return time.Minute
		}
		return e.RetryAfter
	}
	delay := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

// HumanHint turns common failures into advice the user can act on.
func (e *APIError) HumanHint() string {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return "the API key was rejected. Check OPENROUTER_API_KEY in your .env file."
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "this account cannot use that model. Top up credits or pick another model."
	case http.StatusNotFound:
		return "the model does not exist (or is no longer offered). Pick another model."
	case http.StatusTooManyRequests:
		return "rate limited by OpenRouter or the upstream provider. Wait a moment and retry."
	case http.StatusRequestEntityTooLarge:
		return "the audio payload is too large. Lower TRANSCRIBE_CHUNK_SECONDS and retry."
	case http.StatusServiceUnavailable:
		return "no provider is currently serving that model. Try again shortly or pick another model."
	}
	if e.StatusCode >= 500 {
		return "the upstream provider failed. Retrying usually helps."
	}
	return ""
}

// decodeError turns an error response body into an APIError.
func decodeError(status int, body []byte) *APIError {
	apiErr := &APIError{StatusCode: status}

	var payload struct {
		Error struct {
			Code     json.RawMessage `json:"code"`
			Message  string          `json:"message"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		apiErr.Message = strings.TrimSpace(payload.Error.Message)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(payload.Message)
		}
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(payload.Error.Metadata))
		}
		var code int
		if err := json.Unmarshal(payload.Error.Code, &code); err == nil {
			apiErr.Code = code
		} else {
			var codeFloat float64
			if err := json.Unmarshal(payload.Error.Code, &codeFloat); err == nil {
				apiErr.Code = int(codeFloat)
			}
		}
	}

	if apiErr.Message == "" {
		apiErr.Message = snippet(body)
	}
	if len(body) > 0 {
		apiErr.Body = snippet(body)
	}
	return apiErr
}

// retryAfter parses the Retry-After header in either of its two forms.
func retryAfter(header http.Header) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// snippet keeps error text short enough for a dialog.
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "empty response"
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	return text
}
