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
	UUID                      string    `json:"uuid"` // Coolify returns uuid, not id
	ID                        string    `json:"id"`
	Name                      string    `json:"name"`
	Status                    string    `json:"status"` // running | stopped | building | error
	FQDN                      string    `json:"fqdn"`   // Set manually in Coolify UI; API cannot set this
	Repository                string    `json:"repository"`
	Branch                    string    `json:"git_branch"`
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
// across all environments (development-internal, production-internal, etc.).
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
		// Create new service. Endpoint depends on whether a deploy key is configured:
		//   - Private repos → /api/v1/applications/private-deploy-key (requires private_key_uuid)
		//   - Public repos  → /api/v1/applications/public
		// The /public endpoint silently ignores private_key_uuid and private_key_id stays
		// None, causing SSH auth failure at clone time.
		// Response format: {"uuid": "...", "domains": "..."}
		payload := buildCoolifyCreatePayload(app, base)
		privateKeyUUID := app.Spec.Repository.PrivateKeyUUID
		if privateKeyUUID == "" {
			privateKeyUUID = base.Coolify.PrivateKeyUUID
		}
		createEndpoint := "/api/v1/applications/public"
		if privateKeyUUID != "" {
			createEndpoint = "/api/v1/applications/private-deploy-key"
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

// UpdateEnvVars merges the given env var map into the Coolify service configuration.
// Existing vars not in the provided map are preserved.
func (c *Client) UpdateEnvVars(ctx context.Context, serviceID string, vars map[string]string) error {
	// GET existing env vars first
	var existing map[string]string
	if err := c.http.Get(ctx, "/api/v1/applications/"+serviceID+"/envs", &existing); err != nil {
		return fmt.Errorf("get env vars: %w", err)
	}
	// Merge
	for k, v := range vars {
		existing[k] = v
	}
	// PUT merged set
	if err := c.http.Put(ctx, "/api/v1/applications/"+serviceID+"/envs", existing, nil); err != nil {
		return fmt.Errorf("update env vars: %w", err)
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
// Coolify returns statuses like "running", "running:healthy", "running:unhealthy".
func (c *Client) WaitForHealthy(ctx context.Context, serviceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var app App
		if err := c.http.Get(ctx, "/api/v1/applications/"+serviceID, &app); err != nil {
			return err
		}
		// Accept "running" or "running:healthy" — reject "running:unhealthy"
		if app.Status == "running:healthy" || app.Status == "running" {
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
// Used by both /api/v1/applications/public (public repos) and
// /api/v1/applications/private-deploy-key (private repos with SSH deploy key).
// When private_key_uuid is set, the SSH URL is used for git_repository.
func buildCoolifyCreatePayload(app *manifest.AppConfig, base *manifest.BaseConfig) map[string]interface{} {
	// Determine build pack: dockerfile if specified, otherwise nixpacks
	buildPack := "nixpacks"
	if app.Spec.Build.Dockerfile != "" || app.Spec.Build.BaseImage != "" {
		buildPack = "dockerfile"
	}

	// Get environment name based on branch + exposure (development-internal, development, etc.)
	envName := app.GetEnvironmentStage()

	// Resolve the deploy key: app-level overrides base config (per-repo keys).
	// GitHub deploy keys must be unique per repo, so each private repo needs its own key.
	privateKeyUUID := app.Spec.Repository.PrivateKeyUUID
	if privateKeyUUID == "" {
		privateKeyUUID = base.Coolify.PrivateKeyUUID
	}

	// Use SSH URL when a private key is configured; HTTPS for public repos.
	repoURL := app.Spec.Repository.URL
	if privateKeyUUID != "" {
		repoURL = httpsToSSH(repoURL)
	}

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

	// Attach SSH deploy key for private repositories.
	if privateKeyUUID != "" {
		payload["private_key_uuid"] = privateKeyUUID
	}

	return payload
}

// mapEnvironment maps branch + exposure to Coolify environment name
func mapEnvironment(branch, exposure string) string {
	isProduction := branch == "main" || branch == "master"
	isInternal := exposure == "internal"

	switch {
	case isProduction && isInternal:
		return "production-internal"
	case isProduction && !isInternal:
		return "production"
	case !isProduction && isInternal:
		return "development-internal"
	default:
		return "development"
	}
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

// buildFQDN constructs the fully qualified domain name for the app.
// Format for internal apps:
//   - development: http://dev-{app}.apps.mayencenouvelle.internal,http://dev-{app}.internal.apps.mayencenouvelle.com
//   - production: http://{app}.apps.mayencenouvelle.internal,http://{app}.internal.apps.mayencenouvelle.com
// Format for external apps:
//   - development: https://dev-{app}.apps.mayencenouvelle.com
//   - production: https://{app}.apps.mayencenouvelle.com
func buildFQDN(app *manifest.AppConfig) string {
	name := app.Metadata.Name
	isProduction := app.Spec.Repository.Branch == "main" || app.Spec.Repository.Branch == "master"
	exposure := app.Spec.Capabilities.Exposure
	isInternal := exposure == "internal"
	isBoth := exposure == "both"

	// Determine environment prefix
	envPrefix := ""
	if !isProduction {
		envPrefix = "dev-"
	}

	// ── Internal-only apps ──────────────────────────────────────────────────────
	// Coolify fqdn string for internal apps: two comma-separated http:// entries.
	// First: the *.mayencenouvelle.internal domain (Traefik wildcard route).
	// Second: the alt *.internal.apps.mayencenouvelle.com domain (Authentik redirect
	// URIs and users outside the VPN who hit the public DNS alias).
	// The alt domain comes from Domains.External in the manifest if explicitly set,
	// otherwise it is derived from the app name (legacy / default behaviour).
	if isInternal {
		internalDomain := app.Spec.Domains.Internal
		if internalDomain == "" {
			internalDomain = fmt.Sprintf("%s.apps.mayencenouvelle.internal", name)
		}
		altDomain := app.Spec.Domains.External
		if altDomain == "" {
			altDomain = fmt.Sprintf("%s.internal.apps.mayencenouvelle.com", name)
		}
		return fmt.Sprintf("http://%s%s,http://%s%s", envPrefix, internalDomain, envPrefix, altDomain)
	}

	// ── External (public) domain ─────────────────────────────────────────────────
	externalDomain := app.Spec.Domains.External
	if externalDomain == "" {
		externalDomain = fmt.Sprintf("%s.apps.mayencenouvelle.com", name)
	}
	externalFQDN := fmt.Sprintf("https://%s%s", envPrefix, externalDomain)

	// ── Both-zone apps: also include the internal domain ─────────────────────────
	// Use http:// for the internal entry so Coolify→Traefik forwarding works
	// (same convention as pure internal apps).
	if isBoth && app.Spec.Domains.Internal != "" {
		internalFQDN := fmt.Sprintf("http://%s%s", envPrefix, app.Spec.Domains.Internal)
		return fmt.Sprintf("%s,%s", externalFQDN, internalFQDN)
	}

	return externalFQDN
}
