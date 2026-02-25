// Package coolify provides a typed client for the Coolify REST API.
// Coolify API docs: https://coolify.io/docs/api-reference/
//
// Design principles:
//   - All methods are idempotent (safe to call multiple times)
//   - Methods return stable internal types, not raw API responses
//   - API version pinned via Accept header; failures surface as typed errors
package coolify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/common"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
)

// Client is the Coolify API client.
type Client struct {
	http     *common.HTTPClient
	endpoint string
}

// NewClient creates a Coolify API client.
//
//	endpoint: base URL e.g. "https://coolify.apps.mayencenouvelle.internal"
//	token:    Bearer token from Coolify → Settings → API Tokens
func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: endpoint,
		http:     common.NewHTTPClient(endpoint, token),
	}
}

// ─── Types ───────────────────────────────────────────────────────────────────

// App represents a Coolify service/application.
type App struct {
	UUID       string `json:"uuid"` // Coolify returns uuid, not id
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"` // running | stopped | building | error
	FQDN       string `json:"fqdn"`   // Set manually in Coolify UI; API cannot set this
	Repository string `json:"repository"`
	Branch     string `json:"git_branch"`
	// ManualWebhookSecretGithub is Coolify's auto-generated token for this resource.
	// It serves dual purpose:
	//   1. URL token  → {coolify_url}/webhooks/source/github/events/manual?token={value}
	//   2. HMAC secret → Coolify uses it to verify GitHub's X-Hub-Signature-256 header
	// No extra secret management needed — read from Coolify API at deploy time.
	ManualWebhookSecretGithub string    `json:"manual_webhook_secret_github"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

// Deployment represents a single Coolify deployment event.
type Deployment struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // success | failed | running
	Commit    string    `json:"commit"`
	CreatedAt time.Time `json:"created_at"`
}

// PlanAction is a planned change action for dry-run output.
// Returned by Plan* methods.
type PlanAction struct {
	Operation string // create | update | delete | no-change
	Resource  string
	Detail    string
}

// ─── Interface ───────────────────────────────────────────────────────────────

// GetAppByName retrieves the first Coolify service matching the name.
// Returns nil, nil if no service with that name exists.
// Use GetAppByNameAndBranch when both dev and prod resources coexist.
func (c *Client) GetAppByName(ctx context.Context, name string) (*App, error) {
	apps, err := c.GetAppsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, nil
	}
	return &apps[0], nil
}

// GetAppByNameAndBranch retrieves the Coolify service matching both name and git branch.
// Used to disambiguate dev (develop) from prod (main) Coolify resources.
// Returns nil, nil if no matching resource exists.
func (c *Client) GetAppByNameAndBranch(ctx context.Context, name, branch string) (*App, error) {
	apps, err := c.GetAppsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if apps[i].Branch == branch {
			return &apps[i], nil
		}
	}
	return nil, nil
}

// GetAppsByName returns ALL Coolify application resources with the given name
// across all environments (development, production).
// Multiple resources exist when the same app is deployed for both develop and main branches.
// Each matching app is fetched individually to ensure full data (e.g. ManualWebhookSecretGithub
// is omitted from the list response but present in the individual GET response).
func (c *Client) GetAppsByName(ctx context.Context, name string) ([]App, error) {
	var apps []App
	if err := c.http.Get(ctx, "/api/v1/applications", &apps); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	var result []App
	for _, a := range apps {
		if a.Name != name {
			continue
		}
		// Fetch individual app to get full data including ManualWebhookSecretGithub.
		// The list endpoint may omit sensitive fields — individual GET is authoritative.
		var full App
		if err := c.http.Get(ctx, "/api/v1/applications/"+a.UUID, &full); err == nil {
			result = append(result, full)
		} else {
			result = append(result, a) // fallback to partial list data
		}
	}
	return result, nil
}

// EnsureWebhookToken guarantees the Coolify resource has a manual_webhook_secret_github token.
// If the token is already set, it is returned unchanged.
// If missing (e.g. app created without GitHub integration), a 64-char hex token is generated
// and PATCHed to Coolify. The updated token is stored back on the App struct.
func (c *Client) EnsureWebhookToken(ctx context.Context, app *App) (string, error) {
	if app.ManualWebhookSecretGithub != "" {
		return app.ManualWebhookSecretGithub, nil
	}
	// Generate a cryptographically random 64-char hex token (32 bytes → 64 hex chars).
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate webhook token: %w", err)
	}
	hexToken := hex.EncodeToString(raw)
	payload := map[string]interface{}{"manual_webhook_secret_github": hexToken}
	if err := c.http.Patch(ctx, "/api/v1/applications/"+app.UUID, payload, nil); err != nil {
		return "", fmt.Errorf("set webhook token for %s: %w", app.UUID, err)
	}
	app.ManualWebhookSecretGithub = hexToken
	return hexToken, nil
}

// WebhookURL builds the Coolify GitHub webhook delivery URL for an app resource.
// token is the resource's ManualWebhookSecretGithub value.
// The same token is also used as the HMAC signing secret for GitHub's X-Hub-Signature-256.
func (c *Client) WebhookURL(token string) string {
	return c.endpoint + "/webhooks/source/github/events/manual?token=" + token
}

// EnsureApp creates or updates a Coolify service for the given app manifest.
// Idempotent: if the service already exists with the same config, no update is made.
// Requires base config for Coolify UUIDs (project, server, destination).
// Branch-aware: uses name+branch to distinguish dev (develop) from prod (main) resources.
func (c *Client) EnsureApp(ctx context.Context, app *manifest.AppConfig, base *manifest.BaseConfig) (*App, error) {
	existing, err := c.GetAppByNameAndBranch(ctx, app.Metadata.Name, app.Spec.Repository.Branch)
	if err != nil {
		return nil, err
	}

	if existing == nil {
		// Create new service. Endpoint selection priority:
		//   1. GitHub App  → /api/v1/applications/private-github-app (requires github_app_uuid)
		//   2. Deploy key  → /api/v1/applications/private-deploy-key (requires private_key_uuid)
		//   3. Public repo → /api/v1/applications/public
		// GitHub App is preferred: one installation covers all org repos — no per-repo keys.
		// Response format: {"uuid": "...", "domains": "..."}
		payload := buildCoolifyCreatePayload(app, base)
		createEndpoint := "/api/v1/applications/public"
		if base.Coolify.GitHubAppUUID != "" {
			createEndpoint = "/api/v1/applications/private-github-app"
		} else {
			privateKeyUUID := app.Spec.Repository.PrivateKeyUUID
			if privateKeyUUID == "" {
				privateKeyUUID = base.Coolify.PrivateKeyUUID
			}
			if privateKeyUUID != "" {
				createEndpoint = "/api/v1/applications/private-deploy-key"
			}
		}
		var resp struct {
			UUID string `json:"uuid"`
		}
		if err := c.http.Post(ctx, createEndpoint, payload, &resp); err != nil {
			return nil, fmt.Errorf("create application: %w", err)
		}

		// Set the domain immediately after creation via PATCH.
		// The create endpoint ignores the domains field; a separate PATCH is required.
		// Use http:// prefix — https:// causes Coolify to add a redirect-to-https +
		// letsencrypt router which breaks our central-Traefik→coolify-proxy:80 architecture.
		// Traefik labels are regenerated on the subsequent deploy trigger.
		domains := buildFQDN(app)
		domainPayload := map[string]interface{}{"domains": domains}
		if err := c.http.Patch(ctx, "/api/v1/applications/"+resp.UUID, domainPayload, nil); err != nil {
			return nil, fmt.Errorf("set domain after create: %w", err)
		}

		return &App{UUID: resp.UUID, Name: app.Metadata.Name, FQDN: domains}, nil
	}

	// Update existing service.
	// Includes domains so the FQDN stays in sync with the manifest on every deploy.
	// The PATCH response body is empty; return the pre-fetched existing record with
	// updated FQDN so downstream code (e.g. mismatch checks) sees the correct value.
	// Note: git_repository and private_key_uuid cannot be changed via PATCH —
	// resources created with an HTTPS URL must be deleted and recreated.
	domains := buildFQDN(app)
	payload := buildCoolifyUpdatePayload(app, domains)
	if err := c.http.Patch(ctx, "/api/v1/applications/"+existing.UUID, payload, nil); err != nil {
		return nil, fmt.Errorf("update application: %w", err)
	}
	existing.FQDN = domains
	return existing, nil
}

// UpdateEnvVars syncs the desired env var map into a Coolify application.
//
// Algorithm:
//  1. GET existing env vars from Coolify (array of objects).
//  2. For each desired key:
//     - If the key exists with the same value → skip (idempotent).
//     - If the key exists with a different value → DELETE old + POST new.
//     - If the key doesn't exist → POST to create.
//
// Uses DELETE+POST instead of PATCH for updates because Coolify env var UUIDs
// may become stale after app delete/recreate cycles (PATCH returns 404).
// Existing vars not in the desired map are preserved (additive merge).
func (c *Client) UpdateEnvVars(ctx context.Context, appUUID string, desired map[string]string) error {
	basePath := "/api/v1/applications/" + appUUID + "/envs"

	// GET existing env vars — Coolify returns [{uuid, key, value, is_preview, ...}]
	var existing []struct {
		UUID      string `json:"uuid"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		IsPreview bool   `json:"is_preview"`
	}
	if err := c.http.Get(ctx, basePath, &existing); err != nil {
		return fmt.Errorf("get env vars: %w", err)
	}

	// Build lookup: key → {uuid, value} (non-preview entries only)
	type entry struct {
		UUID  string
		Value string
	}
	byKey := make(map[string]entry, len(existing))
	for _, e := range existing {
		if e.IsPreview {
			continue
		}
		byKey[e.Key] = entry{UUID: e.UUID, Value: e.Value}
	}

	for key, value := range desired {
		if e, ok := byKey[key]; ok {
			if e.Value == value {
				continue // already correct
			}
			// Delete stale entry, then create fresh (avoids UUID staleness issues)
			_ = c.http.Delete(ctx, basePath+"/"+e.UUID) // best-effort delete
		}
		// Frontend variables (VITE_*, NEXT_PUBLIC_*) must be available at
		// build-time for static bundles. Keep all others runtime-only by default.
		isBuildTime := strings.HasPrefix(key, "VITE_") || strings.HasPrefix(key, "NEXT_PUBLIC_")
		payload := map[string]interface{}{
			"key":          key,
			"value":        value,
			"is_preview":   false,
			"is_buildtime": isBuildTime,
		}
		if err := c.http.Post(ctx, basePath, payload, nil); err != nil {
			return fmt.Errorf("create env %s: %w", key, err)
		}
	}
	return nil
}

// Deploy triggers an immediate deployment of the given service.
// Uses the /api/v1/deploy endpoint with uuid query parameter.
func (c *Client) Deploy(ctx context.Context, serviceUUID string) error {
	// POST /api/v1/deploy?uuid={uuid}
	if err := c.http.Post(ctx, "/api/v1/deploy?uuid="+serviceUUID, nil, nil); err != nil {
		return fmt.Errorf("trigger deploy: %w", err)
	}
	return nil
}

// ListDeployments returns deployment history for a service, newest first.
func (c *Client) ListDeployments(ctx context.Context, serviceID string) ([]Deployment, error) {
	var deployments []Deployment
	if err := c.http.Get(ctx, "/api/v1/applications/"+serviceID+"/deployments", &deployments); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return deployments, nil
}

// RollbackToDeployment triggers a rollback to a previous deployment ID.
func (c *Client) RollbackToDeployment(ctx context.Context, serviceID, deploymentID string) error {
	payload := map[string]string{"deployment_id": deploymentID}
	if err := c.http.Post(ctx, "/api/v1/applications/"+serviceID+"/rollback", payload, nil); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}

// Stop stops (but does not delete) a running Coolify application.
// The app config and git settings are preserved — it can be restarted from Coolify UI or CLI.
func (c *Client) Stop(ctx context.Context, serviceUUID string) error {
	if err := c.http.Post(ctx, "/api/v1/applications/"+serviceUUID+"/stop", nil, nil); err != nil {
		return fmt.Errorf("stop application: %w", err)
	}
	return nil
}

// Delete permanently removes a Coolify application and all its data.
// This is irreversible — the app must be recreated from scratch via 'deploy'.
func (c *Client) Delete(ctx context.Context, serviceUUID string) error {
	if err := c.http.Delete(ctx, "/api/v1/applications/"+serviceUUID); err != nil {
		return fmt.Errorf("delete application: %w", err)
	}
	return nil
}

// WaitForHealthy polls the service status until it is running or timeout.
// Coolify may return "running", "running:healthy", or "running:unknown"
// depending on whether health checks are configured/parsed.
func (c *Client) WaitForHealthy(ctx context.Context, serviceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var app App
		if err := c.http.Get(ctx, "/api/v1/applications/"+serviceID, &app); err != nil {
			return err
		}
		// Accept running states; reject explicit unhealthy.
		if app.Status == "running:healthy" || app.Status == "running" || app.Status == "running:unknown" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("service %s did not become healthy within %s", serviceID, timeout)
}

// PlanApp returns a dry-run preview of what EnsureApp would change.
func (c *Client) PlanApp(ctx context.Context, app *manifest.AppConfig, base *manifest.BaseConfig) ([]PlanAction, error) {
	existing, err := c.GetAppByNameAndBranch(ctx, app.Metadata.Name, app.Spec.Repository.Branch)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return []PlanAction{
			{Operation: "create", Resource: "Coolify Application", Detail: app.Metadata.Name},
			{Operation: "create", Resource: "Environment Variables", Detail: fmt.Sprintf("%d vars", len(app.Spec.Environment))},
			{Operation: "set", Resource: "Project", Detail: base.Coolify.Project},
			{Operation: "set", Resource: "Environment", Detail: base.Coolify.Environment},
		}, nil
	}
	return []PlanAction{
		{Operation: "update", Resource: "Coolify Application", Detail: fmt.Sprintf("uuid=%s", existing.UUID)},
	}, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// httpsToSSH converts a GitHub HTTPS URL to its SSH equivalent.
// Example: https://github.com/owner/repo.git → git@github.com:owner/repo.git
// Non-GitHub or already-SSH URLs are returned unchanged.
func httpsToSSH(rawURL string) string {
	// Already SSH format
	if len(rawURL) > 4 && rawURL[:4] == "git@" {
		return rawURL
	}
	// https://github.com/owner/repo.git → git@github.com:owner/repo.git
	const prefix = "https://github.com/"
	if len(rawURL) > len(prefix) && rawURL[:len(prefix)] == prefix {
		return "git@github.com:" + rawURL[len(prefix):]
	}
	return rawURL
}

// buildCoolifyCreatePayload builds the JSON payload for creating a Coolify application.
// Supports three modes:
//   - GitHub App  → HTTPS URL + github_app_uuid (preferred — one installation covers all repos)
//   - Deploy key  → SSH URL + private_key_uuid (legacy — per-repo keys)
//   - Public repo → HTTPS URL, no auth fields
func buildCoolifyCreatePayload(app *manifest.AppConfig, base *manifest.BaseConfig) map[string]interface{} {
	// Determine build pack: dockerfile if specified, otherwise nixpacks
	buildPack := "nixpacks"
	if app.Spec.Build.Dockerfile != "" || app.Spec.Build.BaseImage != "" {
		buildPack = "dockerfile"
	}

	// Get environment name based on branch (development or production).
	envName := app.GetEnvironmentStage()

	repoURL := app.Spec.Repository.URL

	payload := map[string]interface{}{
		"project_uuid":        base.Coolify.ProjectUUID,
		"environment_name":    envName,
		"server_uuid":         base.Coolify.ServerUUID,
		"destination_uuid":    base.Coolify.DestinationUUID,
		"git_repository":      repoURL,
		"git_branch":          app.Spec.Repository.Branch,
		"build_pack":          buildPack,
		"ports_exposes":       fmt.Sprintf("%d", app.Spec.Runtime.Port),
		"name":                app.Metadata.Name,
		"dockerfile_location": app.Spec.Build.Dockerfile,
		// domains is NOT set here - Coolify ignores it on the create endpoint.
		// It is set immediately after via a separate PATCH call (see EnsureApp).
	}

	// Priority: GitHub App > legacy deploy key > public
	if base.Coolify.GitHubAppUUID != "" {
		// GitHub App: Coolify clones via the app installation token (HTTPS).
		// The repo URL stays as HTTPS — no SSH conversion needed.
		payload["github_app_uuid"] = base.Coolify.GitHubAppUUID
	} else {
		// Legacy: resolve per-repo or global SSH deploy key.
		privateKeyUUID := app.Spec.Repository.PrivateKeyUUID
		if privateKeyUUID == "" {
			privateKeyUUID = base.Coolify.PrivateKeyUUID
		}
		if privateKeyUUID != "" {
			payload["git_repository"] = httpsToSSH(repoURL)
			payload["private_key_uuid"] = privateKeyUUID
		}
	}

	return payload
}

// mapEnvironment maps branch to Coolify environment name.
// Exposure no longer influences environment selection.
func mapEnvironment(branch, exposure string) string {
	isProduction := branch == "main" || branch == "master"
	if isProduction {
		return "production"
	}
	return "development"
}

// buildCoolifyUpdatePayload builds payload for PATCH /api/v1/applications/{uuid}
// Note: project_uuid, environment_name, server_uuid, destination_uuid are immutable.
// domains must be passed as a pre-built string (from buildFQDN) with http:// prefix
// for internal apps — https:// causes Coolify to generate a redirect+letsencrypt router
// which breaks our central-Traefik→coolify-proxy:80 forwarding architecture.
//
// git_repository and private_key_uuid are intentionally excluded — Coolify's PATCH
// endpoint does not accept private_key_uuid, and changing git_repository without
// re-associating a key would break private repo cloning. Resources created with
// the wrong URL type (HTTPS instead of SSH) must be deleted and recreated via
// 'mn-cli undeploy --delete && mn-cli deploy'.
func buildCoolifyUpdatePayload(app *manifest.AppConfig, domains string) map[string]interface{} {
	// Determine build pack: dockerfile if specified, otherwise nixpacks
	buildPack := "nixpacks"
	if app.Spec.Build.Dockerfile != "" || app.Spec.Build.BaseImage != "" {
		buildPack = "dockerfile"
	}

	return map[string]interface{}{
		"name":                app.Metadata.Name,
		"git_branch":          app.Spec.Repository.Branch,
		"build_pack":          buildPack,
		"ports_exposes":       fmt.Sprintf("%d", app.Spec.Runtime.Port),
		"dockerfile_location": app.Spec.Build.Dockerfile,
		"domains":             domains,
	}
}

// buildFQDN constructs the fully qualified domain list for the app.
// Domain mapping:
//   - domains.public (comma-separated), emitted as http://
//   - domains.private (comma-separated), emitted as http://
//
// Stage prefixing ("dev-") is applied to each hostname entry.
func buildFQDN(app *manifest.AppConfig) string {
	name := app.Metadata.Name
	isProduction := app.Spec.Repository.Branch == "main" || app.Spec.Repository.Branch == "master"
	domains := app.GetDomains()

	// Determine environment prefix
	envPrefix := ""
	if !isProduction {
		envPrefix = "dev-"
	}
	var entries []string
	if domains.Public != "" {
		entries = append(entries, prefixDomainList(domains.Public, envPrefix, "http"))
	}
	if domains.Private != "" {
		entries = append(entries, prefixDomainList(domains.Private, envPrefix, "http"))
	}
	if len(entries) > 0 {
		return strings.Join(entries, ",")
	}
	// Defensive fallback for incomplete manifests.
	return prefixDomainList(fmt.Sprintf("%s.apps.mayencenouvelle.internal", name), envPrefix, "http")
}

func prefixDomainList(raw, envPrefix, scheme string) string {
	parts := strings.Split(raw, ",")
	entries := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		entries = append(entries, fmt.Sprintf("%s://%s%s", scheme, envPrefix, p))
	}
	return strings.Join(entries, ",")
}
