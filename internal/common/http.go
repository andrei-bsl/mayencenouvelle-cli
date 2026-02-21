// Package common provides shared HTTP client utilities for all API clients.
// All API clients (Coolify, Authentik, Traefik, GitHub) use this package
// to ensure consistent: timeout handling, auth headers, error formatting,
// retry-with-backoff, and logging.
package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient wraps net/http.Client with base URL, auth, retries, and logging.
type HTTPClient struct {
	base   string
	token  string
	client *http.Client
}

// NewHTTPClient creates a shared HTTP client.
//
//	base:  base URL including path prefix (e.g. "https://coolify.lab/api/v1")
//	token: Bearer token for Authorization header
func NewHTTPClient(base, token string) *HTTPClient {
	return &HTTPClient{
		base:  base,
		token: token,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Get performs a GET request and decodes the JSON response into out.
func (c *HTTPClient) Get(ctx context.Context, path string, out interface{}) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST request with a JSON body and decodes the response into out.
// out may be nil if no response body is expected.
func (c *HTTPClient) Post(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// Patch performs a PATCH request with a JSON body and decodes the response into out.
func (c *HTTPClient) Patch(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.do(ctx, http.MethodPatch, path, body, out)
}

// Put performs a PUT request with a JSON body.
func (c *HTTPClient) Put(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.do(ctx, http.MethodPut, path, body, out)
}

// Delete performs a DELETE request.
func (c *HTTPClient) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ─── Internal ─────────────────────────────────────────────────────────────────

func (c *HTTPClient) do(ctx context.Context, method, path string, body, out interface{}) error {
	// Encode body
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	// Build request
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Execute with simple retry (3 attempts, exponential backoff)
	var resp *http.Response
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err = c.client.Do(req)
		if err == nil {
			break
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Check status
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       string(raw),
		}
	}

	// Decode response
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}

// ─── Error Types ──────────────────────────────────────────────────────────────

// APIError represents a non-2xx API response.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error: %s %s → HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// IsNotFound returns true if this is a 404 error.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsConflict returns true if this is a 409 conflict (resource already exists).
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}
