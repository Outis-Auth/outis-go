package outis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	maxAttempts     = 4
	retryBase       = 500 * time.Millisecond
	retryCap        = 8 * time.Second
	maxResponseBody = 1 << 20
)

// envelope is every request route's response body.
type envelope struct {
	ServerNow int64   `json:"server_now"`
	Request   Request `json:"request"`
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Kind  string `json:"kind"`
}

// send makes one call to a request route and returns the decoded request
// and the response headers.
func (c *Client) send(ctx context.Context, method, path string, body []byte, header http.Header, retry bool) (*Request, http.Header, error) {
	var env envelope
	hdr, err := c.call(ctx, method, path, body, header, retry, &env)
	if err != nil {
		return nil, nil, err
	}
	return &env.Request, hdr, nil
}

// call makes one API call and decodes a 2xx body into out, retrying 429, 5xx
// and transport failures when retry is set.
func (c *Client) call(ctx context.Context, method, path string, body []byte, header http.Header, retry bool, out any) (http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			var apiErr *APIError
			if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > wait {
				wait = min(apiErr.RetryAfter, 30*time.Second)
			}
			if err := c.clock.Sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		hdr, err := c.once(ctx, method, path, body, header, out)
		if err == nil {
			return hdr, nil
		}
		lastErr = err
		if !retry || !retryable(ctx, err) {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, header http.Header, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, fmt.Errorf("outis: %s %s: %w", method, path, err)
	}
	for k, v := range header {
		hr.Header[k] = v
	}
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("User-Agent", "outis-go/"+Version)
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("outis: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("outis: %s %s: read body: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("outis: %s %s: decode response: %w", method, path, err)
	}
	return resp.Header, nil
}

func apiError(resp *http.Response, raw []byte) *APIError {
	e := &APIError{StatusCode: resp.StatusCode}
	var b errorBody
	if json.Unmarshal(raw, &b) == nil {
		e.Message = b.Error
		e.Code = b.Kind
		if e.Code == "" {
			e.Code = b.Code
		}
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	return true
}

func backoff(attempt int) time.Duration {
	return min(retryBase<<(attempt-1), retryCap)
}
