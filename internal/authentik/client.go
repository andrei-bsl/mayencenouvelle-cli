// Package authentik provides a typed client for the Authentik REST API.
// Authentik API docs: https://docs.goauthentik.io/docs/developer-docs/api/
//
// Design principles:
//   - All provider and application operations are idempotent
//   - Lookup by slug before creating to avoid duplicates
//   - Credentials (client_id, client_secret) surfaced as typed struct
package authentik

import (
	"context"
	"fmt"

	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/common"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
)

// Client is the Authentik API client.
type Client struct {
	http     *common.HTTPClient
	endpoint string
}

// NewClient creates an Authentik API client.
//
//	endpoint: base URL e.g. "https://authentik.apps.mayencenouvelle.internal"
//	token:    API token from Authentik → Admin → Users → Tokens
func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: endpoint,
		http:     common.NewHTTPClient(endpoint+"/api/v3", token),
	}
}

// ─── Types ───────────────────────────────────────────────────────────────────

// OAuth2Provider represents an Authentik OAuth2/OIDC provider.
type OAuth2Provider struct {
	PK           int      `json:"pk"`
	Name         string   `json:"name"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	ClientType   string   `json:"client_type"` // confidential | public
	RedirectURIs string   `json:"redirect_uris"`
	PropertyMappings []int `json:"property_mappings"`
}

// Application represents an Authentik application.
type Application struct {
	PK       string `json:"pk"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Provider int    `json:"provider"`
}

// OIDCCredentials contains the credentials needed by an app to perform OIDC.
type OIDCCredentials struct {
	ProviderName string
	ClientID     string
	ClientSecret string
}

// PlanAction is a planned change for dry-run output.
type PlanAction struct {
	Operation string
	Resource  string
	Detail    string
}

// ─── Interface ───────────────────────────────────────────────────────────────

// EnsureOAuth2Provider creates or updates an OAuth2 provider and linked application.
// Idempotent: resolves existing provider by name before creating.
// Returns the final client credentials.
func (c *Client) EnsureOAuth2Provider(ctx context.Context, app *manifest.AppConfig) (*OIDCCredentials, error) {
	providerName := app.ProviderName()
	redirectURIs := buildRedirectURIs(app)

	// Check if provider already exists
	existing, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return nil, err
	}

	var provider *OAuth2Provider
	if existing == nil {
		// Create: POST /providers/oauth2/
		provider, err = c.createOAuth2Provider(ctx, providerName, redirectURIs, app.Spec.Auth.Scopes)
		if err != nil {
			return nil, fmt.Errorf("create provider: %w", err)
		}
	} else {
		// Update redirect URIs in case domains changed
		provider, err = c.updateOAuth2Provider(ctx, existing.PK, redirectURIs)
		if err != nil {
			return nil, fmt.Errorf("update provider: %w", err)
		}
	}

	// Ensure application is linked to provider
	if err := c.ensureApplication(ctx, app.Metadata.Name, provider.PK); err != nil {
		return nil, fmt.Errorf("ensure application: %w", err)
	}

	return &OIDCCredentials{
		ProviderName: providerName,
		ClientID:     provider.ClientID,
		ClientSecret: provider.ClientSecret,
	}, nil
}

// RotateOAuth2Secret generates a new client secret for an existing provider.
// The client_id remains the same; only client_secret is rotated.
func (c *Client) RotateOAuth2Secret(ctx context.Context, appName string) (*OIDCCredentials, error) {
	providerName := appName + "-oauth2"
	provider, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("provider %s not found in Authentik", providerName)
	}

	// POST /providers/oauth2/{pk}/rotate_secret/
	var rotated OAuth2Provider
	if err := c.http.Post(ctx,
		fmt.Sprintf("/providers/oauth2/%d/rotate_secret/", provider.PK),
		nil, &rotated,
	); err != nil {
		return nil, fmt.Errorf("rotate secret: %w", err)
	}

	return &OIDCCredentials{
		ProviderName: providerName,
		ClientID:     rotated.ClientID,
		ClientSecret: rotated.ClientSecret,
	}, nil
}

// PlanOAuth2Provider returns a dry-run preview for EnsureOAuth2Provider.
func (c *Client) PlanOAuth2Provider(ctx context.Context, app *manifest.AppConfig) ([]PlanAction, error) {
	providerName := app.ProviderName()
	existing, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return nil, err
	}

	if existing == nil {
		return []PlanAction{
			{Operation: "create", Resource: "OAuth2 Provider", Detail: providerName},
			{Operation: "create", Resource: "Application", Detail: app.Metadata.Name},
		}, nil
	}
	return []PlanAction{
		{Operation: "update", Resource: "OAuth2 Provider", Detail: fmt.Sprintf("%s (pk=%d)", providerName, existing.PK)},
		{Operation: "no-change", Resource: "Application", Detail: app.Metadata.Name},
	}, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

func (c *Client) getProviderByName(ctx context.Context, name string) (*OAuth2Provider, error) {
	// GET /providers/oauth2/?search=<name>
	var result struct {
		Results []OAuth2Provider `json:"results"`
	}
	if err := c.http.Get(ctx, "/providers/oauth2/?search="+name, &result); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	for _, p := range result.Results {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, nil
}

func (c *Client) createOAuth2Provider(ctx context.Context, name, redirectURIs string, scopes []string) (*OAuth2Provider, error) {
	payload := map[string]interface{}{
		"name":          name,
		"client_type":   "confidential",
		"redirect_uris": redirectURIs,
		"issuer_mode":   "per_provider",
		// property_mappings resolved to openid, profile, email scope objects
	}
	var provider OAuth2Provider
	if err := c.http.Post(ctx, "/providers/oauth2/", payload, &provider); err != nil {
		return nil, err
	}
	return &provider, nil
}

func (c *Client) updateOAuth2Provider(ctx context.Context, pk int, redirectURIs string) (*OAuth2Provider, error) {
	payload := map[string]interface{}{
		"redirect_uris": redirectURIs,
	}
	var provider OAuth2Provider
	if err := c.http.Patch(ctx, fmt.Sprintf("/providers/oauth2/%d/", pk), payload, &provider); err != nil {
		return nil, err
	}
	return &provider, nil
}

func (c *Client) ensureApplication(ctx context.Context, name string, providerPK int) error {
	slug := name
	// Check if application exists
	var result struct {
		Results []Application `json:"results"`
	}
	if err := c.http.Get(ctx, "/core/applications/?search="+slug, &result); err != nil {
		return err
	}
	for _, a := range result.Results {
		if a.Slug == slug {
			return nil // Already exists and linked
		}
	}
	// Create
	payload := map[string]interface{}{
		"name":     name,
		"slug":     slug,
		"provider": providerPK,
	}
	if err := c.http.Post(ctx, "/core/applications/", payload, nil); err != nil {
		return err
	}
	return nil
}

func buildRedirectURIs(app *manifest.AppConfig) string {
	var uris []string
	for _, path := range app.Spec.Auth.RedirectPaths {
		if app.Spec.Domains.Internal != "" {
			uris = append(uris, fmt.Sprintf("https://%s%s", app.Spec.Domains.Internal, path))
		}
		if app.Spec.Domains.External != "" {
			uris = append(uris, fmt.Sprintf("https://%s%s", app.Spec.Domains.External, path))
		}
	}
	// Authentik accepts newline-separated URIs
	result := ""
	for i, u := range uris {
		if i > 0 {
			result += "\n"
		}
		result += u
	}
	return result
}
