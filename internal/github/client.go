// Package github provides a typed client for the GitHub REST API,
// specifically focused on webhook management for Coolify CI/CD integration.
package github

import (
"context"
"fmt"
"strings"

"github.com/mayencenouvelle/mayencenouvelle-cli/internal/common"
)

// Client is the GitHub API client.
type Client struct {
	http *common.HTTPClient
}

// NewClient creates a GitHub API client.
//
//	token: GitHub personal access token (scope: admin:repo_hook)
func NewClient(token string) *Client {
	return &Client{
		http: common.NewHTTPClient("https://api.github.com", token),
	}
}

// ─── Types ───────────────────────────────────────────────────────────────────

// Webhook represents a GitHub repository webhook.
type Webhook struct {
	ID     int64         `json:"id"`
	URL    string        `json:"url"`
	Events []string      `json:"events"`
	Active bool          `json:"active"`
	Config WebhookConfig `json:"config"`
}

// WebhookConfig holds webhook delivery configuration.
type WebhookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	InsecureSSL string `json:"insecure_ssl"`
	Secret      string `json:"secret,omitempty"`
}

// ─── Interface ───────────────────────────────────────────────────────────────

// EnsureWebhook creates or updates a webhook on the given repository.
// Idempotent: looks up existing webhook by URL; creates if absent, patches if present.
// Returns (webhook, created, error). created=true when the webhook was newly registered.
//
//	repo:       "org/repo" format — use RepoSlug() to derive from a git URL
//	webhookURL: the Coolify webhook URL to deliver to
//	secret:     HMAC signing secret; use Coolify's ManualWebhookSecretGithub value
//	            so Coolify can verify GitHub's X-Hub-Signature-256 header
func (c *Client) EnsureWebhook(ctx context.Context, repo, webhookURL, secret string) (*Webhook, bool, error) {
	existing, err := c.findWebhookByURL(ctx, repo, webhookURL)
	if err != nil {
		return nil, false, err
	}

	payload := map[string]interface{}{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": WebhookConfig{
			URL:         webhookURL,
			ContentType: "json",
			InsecureSSL: "0",
			Secret:      secret,
		},
	}

	if existing == nil {
		var created Webhook
		if err := c.http.Post(ctx, "/repos/"+repo+"/hooks", payload, &created); err != nil {
			return nil, false, fmt.Errorf("create webhook: %w", err)
		}
		return &created, true, nil
	}

	// Update to ensure active + rotate secret on every deploy (idempotent)
	var updated Webhook
	if err := c.http.Patch(ctx,
fmt.Sprintf("/repos/%s/hooks/%d", repo, existing.ID),
payload, &updated,
	); err != nil {
		return nil, false, fmt.Errorf("update webhook: %w", err)
	}
	return &updated, false, nil
}

// DeleteWebhookByURL removes a webhook from a repository, identified by its delivery URL.
// No-op if the webhook does not exist (idempotent).
func (c *Client) DeleteWebhookByURL(ctx context.Context, repo, webhookURL string) error {
	hook, err := c.findWebhookByURL(ctx, repo, webhookURL)
	if err != nil {
		return err
	}
	if hook == nil {
		return nil // already gone
	}
	if err := c.http.Delete(ctx, fmt.Sprintf("/repos/%s/hooks/%d", repo, hook.ID)); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	return nil
}

// VerifyWebhook checks that a webhook exists and is active on the repository.
// Returns the webhook if found and active, nil if not found.
func (c *Client) VerifyWebhook(ctx context.Context, repo, webhookURL string) (*Webhook, error) {
	hook, err := c.findWebhookByURL(ctx, repo, webhookURL)
	if err != nil {
		return nil, err
	}
	if hook == nil {
		return nil, nil
	}
	if !hook.Active {
		return nil, fmt.Errorf("webhook found but inactive: repo=%s url=%s", repo, webhookURL)
	}
	return hook, nil
}

// ListWebhooks returns all webhooks on a repository.
func (c *Client) ListWebhooks(ctx context.Context, repo string) ([]Webhook, error) {
	var hooks []Webhook
	if err := c.http.Get(ctx, "/repos/"+repo+"/hooks", &hooks); err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	return hooks, nil
}

// RepoSlug extracts the "org/repo" slug from a GitHub remote URL.
// Handles both HTTPS ("https://github.com/org/repo.git") and
// SSH ("git@github.com:org/repo.git") formats.
func RepoSlug(repoURL string) string {
	// HTTPS: https://github.com/org/repo[.git]
	if i := strings.Index(repoURL, "github.com/"); i >= 0 {
		slug := repoURL[i+len("github.com/"):]
		return strings.TrimSuffix(slug, ".git")
	}
	// SSH: git@github.com:org/repo[.git]
	if i := strings.Index(repoURL, "github.com:"); i >= 0 {
		slug := repoURL[i+len("github.com:"):]
		return strings.TrimSuffix(slug, ".git")
	}
	return repoURL
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

func (c *Client) findWebhookByURL(ctx context.Context, repo, targetURL string) (*Webhook, error) {
	hooks, err := c.ListWebhooks(ctx, repo)
	if err != nil {
		return nil, err
	}
	for _, h := range hooks {
		if h.Config.URL == targetURL {
			return &h, nil
		}
	}
	return nil, nil
}
