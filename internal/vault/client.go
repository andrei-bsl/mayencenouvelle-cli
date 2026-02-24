// Package vault provides a lightweight OpenBao (Vault-compatible) KV v2 client.
// It handles secret read/write operations within a configurable namespace,
// following the same HTTP patterns as other internal API clients.
//
// This client is used by mn-cli to persist Authentik OIDC credentials (client_id,
// client_secret) after app deployment, storing them at:
//
//	mn/{category}/{app-name}
//
// where {category} comes from metadata.category in the app manifest.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a lightweight OpenBao KV v2 client.
type Client struct {
	addr      string // e.g. "https://vault.mayencenouvelle.internal"
	token     string // BAO_TOKEN
	namespace string // BAO_NAMESPACE (sent as X-Vault-Namespace header)
	client    *http.Client
}

// NewClient creates a new vault client.
// addr is the OpenBao base URL, token is the authentication token,
// namespace is the OpenBao namespace (e.g. "mayencenouvelle").
func NewClient(addr, token, namespace string) *Client {
	return &Client{
		addr:      addr,
		token:     token,
		namespace: namespace,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // self-signed certs in homelab
				},
			},
		},
	}
}

// Enabled returns true if the vault client has enough config to operate.
// When false, vault operations should be silently skipped (graceful degradation).
func (c *Client) Enabled() bool {
	return c.addr != "" && c.token != ""
}

// KVRead reads a secret from the KV v2 engine.
// path is the full API path including "data/" prefix, e.g. "mn/data/apps/hello-world".
// Returns the secret data map, or nil if the secret doesn't exist.
func (c *Client) KVRead(ctx context.Context, path string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v1/%s", c.addr, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("vault: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // secret doesn't exist yet
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vault: GET %s → HTTP %d: %s", path, resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Data map[string]interface{} `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vault: decode response: %w", err)
	}
	return result.Data.Data, nil
}

// KVWrite writes or updates a secret in the KV v2 engine.
// path is the full API path including "data/" prefix, e.g. "mn/data/apps/hello-world".
// data is the key-value map to store. Existing keys not in data are preserved
// via a read-merge-write pattern.
func (c *Client) KVWrite(ctx context.Context, path string, data map[string]string) error {
	// Read-merge-write: preserve existing keys that aren't being overwritten
	existing, err := c.KVRead(ctx, path)
	if err != nil {
		// Non-fatal: if read fails, just write the new data
		existing = nil
	}

	merged := make(map[string]interface{})
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range data {
		merged[k] = v
	}

	payload := map[string]interface{}{
		"data": merged,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("vault: marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s", c.addr, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vault: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault: POST %s → HTTP %d: %s", path, resp.StatusCode, string(raw))
	}

	return nil
}

// setHeaders adds auth and namespace headers to the request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("X-Vault-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}
}
