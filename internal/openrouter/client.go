// Package openrouter is a small client for the parts of the OpenRouter API this
// application needs: listing speech-to-text models and submitting audio for
// transcription.
//
// Transcription uses POST /api/v1/audio/transcriptions with a JSON body, as
// documented at https://openrouter.ai/docs/api/reference/transcription.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the public API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// RequestTimeout bounds a single attempt. OpenRouter stops an upstream
// transcription request after about 60 seconds, so this only ever fires when
// the network itself has stalled.
const RequestTimeout = 5 * time.Minute

// Client talks to the OpenRouter HTTP API.
type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client

	// MaxAttempts counts the first try. Zero means three.
	MaxAttempts int

	// OnRetry is called before each backoff sleep; it may be nil.
	OnRetry func(attempt int, delay time.Duration, err error)

	// sleep is swapped out by tests to skip the backoff.
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a client authenticated with apiKey.
func New(apiKey string) *Client {
	return &Client{
		APIKey:      apiKey,
		BaseURL:     DefaultBaseURL,
		HTTP:        &http.Client{Timeout: RequestTimeout},
		MaxAttempts: 3,
		sleep:       sleepContext,
	}
}

// do performs one API call and returns the raw response body. Transient
// failures (timeouts, 429, 5xx) are retried with exponential backoff.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		payload = encoded
	}

	attempts := c.MaxAttempts
	if attempts < 1 {
		attempts = 3
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		data, err := c.once(ctx, method, path, payload)
		if err == nil {
			return data, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == attempts || !retryable(err) {
			return nil, err
		}

		delay := retryDelay(err, attempt)
		if c.OnRetry != nil {
			c.OnRetry(attempt, delay, err)
		}
		if err := c.wait(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setHeaders(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &networkError{err: err}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, &networkError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := decodeError(resp.StatusCode, data)
		if after, ok := retryAfter(resp.Header); ok {
			apiErr.RetryAfter = after
		}
		return nil, apiErr
	}
	return data, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	// Attribution headers recommended by OpenRouter; harmless otherwise.
	req.Header.Set("X-Title", "Cloud Transcribe")
	req.Header.Set("HTTP-Referer", "https://localhost/cloud-transcribe")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// networkError marks a transport failure, which is always worth retrying: DNS
// hiccups and dropped connections are the usual causes.
type networkError struct{ err error }

func (e *networkError) Error() string {
	text := e.err.Error()
	if strings.Contains(text, "no such host") {
		return "cannot reach the OpenRouter API (check your internet connection): " + text
	}
	return text
}

func (e *networkError) Unwrap() error { return e.err }

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	var netErr *networkError
	return errors.As(err, &netErr)
}

func retryDelay(err error, attempt int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryDelay(attempt)
	}
	delay := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}
