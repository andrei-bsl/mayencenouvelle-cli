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
		// Create new service via /api/v1/applications/public endpoint
		// Response format: {"uuid": "...", "domains": "..."}
		payload := buildCoolifyCreatePayload(app, base)
		var resp struct {
			UUID string `json:"uuid"`
		}
		if err := c.http.Post(ctx, "/api/v1/applications/public", payload, &resp); err != nil {
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
		{Operation: "update", Resource: "Coolify Application", Detail: fmt.Sprintf("id=%s", existing.ID)},
	}, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// buildCoolifyCreatePayload builds payload for POST /api/v1/applications/public
func buildCoolifyCreatePayload(app *manifest.AppConfig, base *manifest.BaseConfig) map[string]interface{} {
	// Determine build pack: dockerfile if specified, otherwise nixpacks
	buildPack := "nixpacks"
	if app.Spec.Build.Dockerfile != "" || app.Spec.Build.BaseImage != "" {
		buildPack = "dockerfile"
	}

	// Get environment name based on branch + exposure (development-internal, development, etc.)
	envName := app.GetEnvironmentStage()

	return map[string]interface{}{
		// Required fields for /api/v1/applications/public
		"project_uuid":     base.Coolify.ProjectUUID,
		"environment_name": envName,
		"server_uuid":      base.Coolify.ServerUUID,
		"destination_uuid": base.Coolify.DestinationUUID,
		"git_repository":   app.Spec.Repository.URL,
		"git_branch":       app.Spec.Repository.Branch,
		"build_pack":       buildPack,
		"ports_exposes":    fmt.Sprintf("%d", app.Spec.Runtime.Port),
		"name":             app.Metadata.Name,

		// Optional fields
		"dockerfile_location": app.Spec.Build.Dockerfile,
			// domains is NOT set here - Coolify ignores it on the create endpoint.
		// It is set immediately after via a separate PATCH call (see EnsureApp).
	}
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
	isInternal := app.Spec.Capabilities.Exposure == "internal"

	// Determine environment prefix
	envPrefix := ""
	if !isProduction {
		envPrefix = "dev-"
	}

	// Use explicit domain from manifest if provided, with env prefix
	if isInternal && app.Spec.Domains.Internal != "" {
		domain := app.Spec.Domains.Internal
		// Prepend env prefix if not production
		if envPrefix != "" {
			domain = envPrefix + domain
		}
		// For internal apps, also add the alternate domain
		// Extract just the app name part and construct alternate domain
		altDomain := fmt.Sprintf("%s%s.internal.apps.mayencenouvelle.com", envPrefix, name)
		return fmt.Sprintf("http://%s,http://%s", domain, altDomain)
	}

	if !isInternal && app.Spec.Domains.External != "" {
		domain := app.Spec.Domains.External
		if envPrefix != "" {
			domain = envPrefix + domain
		}
		return fmt.Sprintf("https://%s", domain)
	}

	// Generate default domain based on exposure
	if isInternal {
		return fmt.Sprintf("http://%s%s.apps.mayencenouvelle.internal,http://%s%s.internal.apps.mayencenouvelle.com",
			envPrefix, name, envPrefix, name)
	}
	return fmt.Sprintf("https://%s%s.apps.mayencenouvelle.com", envPrefix, name)
}
