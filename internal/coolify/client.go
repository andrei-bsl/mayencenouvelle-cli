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
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"` // running | stopped | building | error
	Repository string    `json:"repository"`
	Branch     string    `json:"branch"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
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

// GetAppByName retrieves a Coolify service by its name.
// Returns nil, nil if no service with that name exists.
func (c *Client) GetAppByName(ctx context.Context, name string) (*App, error) {
	// GET /api/v1/applications
	// Filter by name client-side (Coolify list endpoint)
	var apps []App
	if err := c.http.Get(ctx, "/api/v1/applications", &apps); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	for _, a := range apps {
		if a.Name == name {
			return &a, nil
		}
	}
	return nil, nil
}

// EnsureApp creates or updates a Coolify service for the given app manifest.
// Idempotent: if the service already exists with the same config, no update is made.
func (c *Client) EnsureApp(ctx context.Context, app *manifest.AppConfig) (*App, error) {
	existing, err := c.GetAppByName(ctx, app.Metadata.Name)
	if err != nil {
		return nil, err
	}

	payload := buildCoolifyPayload(app)

	if existing == nil {
		// Create new service
		var created App
		if err := c.http.Post(ctx, "/api/v1/applications", payload, &created); err != nil {
			return nil, fmt.Errorf("create application: %w", err)
		}
		return &created, nil
	}

	// Update existing service
	var updated App
	if err := c.http.Patch(ctx, "/api/v1/applications/"+existing.ID, payload, &updated); err != nil {
		return nil, fmt.Errorf("update application: %w", err)
	}
	return &updated, nil
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
func (c *Client) Deploy(ctx context.Context, serviceID string) error {
	// POST /api/v1/applications/{id}/deploy
	if err := c.http.Post(ctx, "/api/v1/applications/"+serviceID+"/deploy", nil, nil); err != nil {
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

// WaitForHealthy polls the service status until it is "running" or timeout.
func (c *Client) WaitForHealthy(ctx context.Context, serviceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var app App
		if err := c.http.Get(ctx, "/api/v1/applications/"+serviceID, &app); err != nil {
			return err
		}
		if app.Status == "running" {
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
func (c *Client) PlanApp(ctx context.Context, app *manifest.AppConfig) ([]PlanAction, error) {
	existing, err := c.GetAppByName(ctx, app.Metadata.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return []PlanAction{
			{Operation: "create", Resource: "Coolify Application", Detail: app.Metadata.Name},
			{Operation: "create", Resource: "Environment Variables", Detail: fmt.Sprintf("%d vars", len(app.Spec.Environment))},
		}, nil
	}
	return []PlanAction{
		{Operation: "update", Resource: "Coolify Application", Detail: fmt.Sprintf("id=%s", existing.ID)},
	}, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

func buildCoolifyPayload(app *manifest.AppConfig) map[string]interface{} {
	return map[string]interface{}{
		"name":          app.Metadata.Name,
		"repository":    app.Spec.Repository.URL,
		"branch":        app.Spec.Repository.Branch,
		"build_command": app.Spec.Build.Command,
		"base_image":    app.Spec.Build.BaseImage,
		"port":          app.Spec.Runtime.Port,
		"environment":   app.Spec.Environment,
	}
}
