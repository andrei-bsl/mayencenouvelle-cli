// Package authentik provides a typed client for the Authentik REST API.
// Authentik API docs: https://docs.goauthentik.io/docs/developer-docs/api/
//
// Naming convention for CLI-managed resources (kebab-case, mn- prefix):
//
//	Provider name : mn-{app-name}-provider   (e.g. mn-vpn-app-provider)
//	Application slug: mn-{app-name}          (e.g. mn-vpn-app)
//	Application name: title-cased display    (e.g. Vpn App)
//
// All operations are idempotent — safe to run on every deploy.
package authentik

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/common"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
)

// Client is the Authentik API client.
type Client struct {
	http *common.HTTPClient
}

// NewClient creates an Authentik API client.
//
//	url:   base URL of the Authentik instance, e.g. "https://auth.mayencenouvelle.com"
//	token: API token from Authentik → Admin → Users → Service Accounts
//
// The /api/v3 suffix is appended automatically (mirrors how the Coolify client
// appends /api/v1 to COOLIFY_URL).
func NewClient(url, token string) *Client {
	return &Client{
		http: common.NewHTTPClient(url+"/api/v3", token),
	}
}

// ─── Types ───────────────────────────────────────────────────────────────────

// RedirectURI represents a single redirect URI entry in Authentik's format.
type RedirectURI struct {
	MatchingMode string `json:"matching_mode"` // "strict" | "regex"
	URL          string `json:"url"`
}

// OAuth2Provider represents an Authentik OAuth2/OIDC provider.
type OAuth2Provider struct {
	PK               int           `json:"pk"`
	Name             string        `json:"name"`
	ClientID         string        `json:"client_id"`
	ClientSecret     string        `json:"client_secret"`
	ClientType       string        `json:"client_type"` // "confidential" | "public"
	SigningKey       string        `json:"signing_key"`
	RedirectURIs     []RedirectURI `json:"redirect_uris"`
	PropertyMappings []string      `json:"property_mappings"`
}

// Application represents an Authentik application.
type Application struct {
	PK       string `json:"pk"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Provider *int   `json:"provider"` // null when no provider linked
	Group    string `json:"group"`
}

// OIDCCredentials contains the credentials surfaced to the Coolify env injector.
type OIDCCredentials struct {
	ProviderName string
	ClientID     string
	ClientSecret string
	// Created indicates whether a new provider was created (false = updated/unchanged).
	Created bool
}

// PlanAction is a planned change for dry-run output.
type PlanAction struct {
	Operation string
	Resource  string
	Detail    string
}

// ─── Public API ──────────────────────────────────────────────────────────────

// EnsureOAuth2Provider creates or reconciles an OAuth2 provider and linked Authentik
// application for the given app manifest. Idempotent and convergent:
//
//   - If neither the provider nor the app exist → create both
//   - If the provider exists but redirect URIs drifted → PATCH to reconcile
//   - If the app exists but is not linked to the provider → PATCH to link
//   - If everything is aligned → no API writes
//
// Naming (kebab-case, mn- prefix):
//
//	Provider: mn-{app-name}-provider
//	App slug: mn-{app-name}
func (c *Client) EnsureOAuth2Provider(
	ctx context.Context,
	app *manifest.AppConfig,
	base *manifest.BaseConfig,
) (*OIDCCredentials, error) {
	providerName := app.ProviderName()
	appSlug := app.AppSlug()
	desiredURIs := buildRedirectURIs(app, base)

	// ── 1. Resolve or create OAuth2 provider ──────────────────────────────
	existingProvider, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return nil, fmt.Errorf("lookup provider: %w", err)
	}

	var provider *OAuth2Provider
	created := false

	if existingProvider == nil {
		// New provider — resolve property mapping UUIDs from base config
		mappings := resolvePropertyMappings(app.Spec.Auth.Scopes, base)
		provider, err = c.createOAuth2Provider(ctx, providerName, desiredURIs, mappings, app.OAuth2ClientType(), base)
		if err != nil {
			return nil, fmt.Errorf("create provider: %w", err)
		}
		created = true
	} else {
		// Reconcile redirect URIs if drifted
		if !redirectURIsEqual(existingProvider.RedirectURIs, desiredURIs) {
			provider, err = c.patchProviderRedirectURIs(ctx, existingProvider.PK, desiredURIs)
			if err != nil {
				return nil, fmt.Errorf("update provider redirect URIs: %w", err)
			}
		} else {
			provider = existingProvider
		}
	}

	// ── 2. Resolve or create Authentik application ────────────────────────
	existingApp, err := c.getApplicationBySlug(ctx, appSlug)
	if err != nil {
		return nil, fmt.Errorf("lookup application: %w", err)
	}

	if existingApp == nil {
		if err := c.createApplication(ctx, app, provider.PK); err != nil {
			return nil, fmt.Errorf("create application: %w", err)
		}
	} else {
		// Re-link to provider if needed (e.g. provider was recreated)
		providerMismatch := existingApp.Provider == nil || *existingApp.Provider != provider.PK
		groupMismatch := existingApp.Group != app.AuthentikGroup()
		if providerMismatch || groupMismatch {
			if err := c.patchApplication(ctx, appSlug, provider.PK, app.AuthentikGroup()); err != nil {
				return nil, fmt.Errorf("update application: %w", err)
			}
		}
	}

	return &OIDCCredentials{
		ProviderName: providerName,
		ClientID:     provider.ClientID,
		ClientSecret: provider.ClientSecret,
		Created:      created,
	}, nil
}

// DeleteOIDC removes the Authentik OAuth2 provider and application for the named app.
// Looks up resources by the canonical mn-{name} slug and mn-{name}-provider name.
// Safe to call when resources don't exist (no-op).
func (c *Client) DeleteOIDC(ctx context.Context, appName string) error {
	slug := "mn-" + appName
	providerName := "mn-" + appName + "-provider"

	// Delete application first (provider can't be deleted while it's referenced)
	app, err := c.getApplicationBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("lookup application: %w", err)
	}
	if app != nil {
		if err := c.http.Delete(ctx, "/core/applications/"+slug+"/"); err != nil {
			return fmt.Errorf("delete application: %w", err)
		}
	}

	// Then delete the provider
	provider, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return fmt.Errorf("lookup provider: %w", err)
	}
	if provider != nil {
		if err := c.http.Delete(ctx, fmt.Sprintf("/providers/oauth2/%d/", provider.PK)); err != nil {
			return fmt.Errorf("delete provider: %w", err)
		}
	}

	return nil
}

// RotateOAuth2Secret generates a new client secret for an existing provider.
// The client_id remains the same; only client_secret is rotated.
func (c *Client) RotateOAuth2Secret(ctx context.Context, appName string) (*OIDCCredentials, error) {
	providerName := "mn-" + appName + "-provider"
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
func (c *Client) PlanOAuth2Provider(ctx context.Context, app *manifest.AppConfig, base *manifest.BaseConfig) ([]PlanAction, error) {
	providerName := app.ProviderName()
	appSlug := app.AppSlug()

	existingProvider, err := c.getProviderByName(ctx, providerName)
	if err != nil {
		return nil, err
	}
	existingApp, err := c.getApplicationBySlug(ctx, appSlug)
	if err != nil {
		return nil, err
	}

	var actions []PlanAction

	if existingProvider == nil {
		actions = append(actions, PlanAction{Operation: "create", Resource: "OAuth2 Provider", Detail: providerName})
	} else {
		desiredURIs := buildRedirectURIs(app, base)
		if !redirectURIsEqual(existingProvider.RedirectURIs, desiredURIs) {
			actions = append(actions, PlanAction{
				Operation: "update",
				Resource:  "OAuth2 Provider",
				Detail:    fmt.Sprintf("%s (pk=%d) — redirect URIs drifted", providerName, existingProvider.PK),
			})
		} else {
			actions = append(actions, PlanAction{Operation: "no-change", Resource: "OAuth2 Provider", Detail: providerName})
		}
	}

	if existingApp == nil {
		actions = append(actions, PlanAction{Operation: "create", Resource: "Application", Detail: fmt.Sprintf("%s (slug=%s)", app.AppDisplayName(), appSlug)})
	} else {
		actions = append(actions, PlanAction{Operation: "no-change", Resource: "Application", Detail: appSlug})
	}

	return actions, nil
}

// ─── Private helpers ──────────────────────────────────────────────────────────

func (c *Client) getProviderByName(ctx context.Context, name string) (*OAuth2Provider, error) {
	var result struct {
		Results []OAuth2Provider `json:"results"`
	}
	if err := c.http.Get(ctx, "/providers/oauth2/?search="+name, &result); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	for i := range result.Results {
		if result.Results[i].Name == name {
			return &result.Results[i], nil
		}
	}
	return nil, nil
}

func (c *Client) getApplicationBySlug(ctx context.Context, slug string) (*Application, error) {
	var result struct {
		Results []Application `json:"results"`
	}
	if err := c.http.Get(ctx, "/core/applications/?search="+slug, &result); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	for i := range result.Results {
		if result.Results[i].Slug == slug {
			return &result.Results[i], nil
		}
	}
	return nil, nil
}

func (c *Client) createOAuth2Provider(
	ctx context.Context,
	name string,
	redirectURIs []RedirectURI,
	propertyMappings []string,
	clientType string,
	base *manifest.BaseConfig,
) (*OAuth2Provider, error) {
	payload := map[string]interface{}{
		"name":               name,
		"client_type":        clientType,
		"redirect_uris":      redirectURIs,
		"issuer_mode":        "per_provider",
		"authorization_flow": base.Authentik.AuthorizationFlow,
		"invalidation_flow":  base.Authentik.InvalidationFlow,
		"property_mappings":  propertyMappings,
	}
	if base.Authentik.SigningKey != "" {
		payload["signing_key"] = base.Authentik.SigningKey
	}
	var provider OAuth2Provider
	if err := c.http.Post(ctx, "/providers/oauth2/", payload, &provider); err != nil {
		return nil, err
	}
	return &provider, nil
}

func (c *Client) patchProviderRedirectURIs(ctx context.Context, pk int, redirectURIs []RedirectURI) (*OAuth2Provider, error) {
	payload := map[string]interface{}{
		"redirect_uris": redirectURIs,
	}
	var provider OAuth2Provider
	if err := c.http.Patch(ctx, fmt.Sprintf("/providers/oauth2/%d/", pk), payload, &provider); err != nil {
		return nil, err
	}
	return &provider, nil
}

func (c *Client) createApplication(ctx context.Context, app *manifest.AppConfig, providerPK int) error {
	payload := map[string]interface{}{
		"name":     app.AppDisplayName(),
		"slug":     app.AppSlug(),
		"provider": providerPK,
		"group":    app.AuthentikGroup(),
	}
	return c.http.Post(ctx, "/core/applications/", payload, nil)
}

func (c *Client) patchApplication(ctx context.Context, slug string, providerPK int, group string) error {
	payload := map[string]interface{}{
		"provider": providerPK,
		"group":    group,
	}
	return c.http.Patch(ctx, "/core/applications/"+slug+"/", payload, nil)
}

// buildRedirectURIs builds the full list of redirect URIs for an app.
//
// If spec.authentication.redirect_uris is non-empty, those exact strings are
// used verbatim (all strict matching) and all auto-derivation is skipped.
//
// Otherwise the URIs are derived automatically from the app's domains:
//   - Stage-prefixed private/public domain lists
//   - Canonical private/public domain lists
//   - Localhost entries if spec.authentication.localhost_ports is set
//
// In both cases, any entries in spec.authentication.redirect_uris_regex are
// appended as regex-mode URIs. This allows a single pattern to cover multiple
// paths (e.g. all Swagger module pages) without enumerating each sub-path.
//
// This produces a URI set that covers dev, prod, and local development from a single provider.
func buildRedirectURIs(app *manifest.AppConfig, base *manifest.BaseConfig) []RedirectURI {
	appendRegex := func(uris []RedirectURI) []RedirectURI {
		for _, u := range app.Spec.Auth.RedirectURIsRegex {
			uris = append(uris, RedirectURI{MatchingMode: "regex", URL: u})
		}
		return uris
	}

	// Explicit override: use manifest-specified URIs verbatim.
	if len(app.Spec.Auth.RedirectURIs) > 0 {
		uris := make([]RedirectURI, 0, len(app.Spec.Auth.RedirectURIs))
		for _, u := range app.Spec.Auth.RedirectURIs {
			uris = append(uris, RedirectURI{MatchingMode: "strict", URL: u})
		}
		return appendRegex(uris)
	}
	stageDomains := app.GetDomains()       // stage-aware (dev-* prefix for develop branch)
	specDomains := app.NormalizedDomains() // canonical (no dev- prefix)
	redirectPaths := app.Spec.Auth.RedirectPaths
	if len(redirectPaths) == 0 {
		// Default callback path keeps OIDC provisioning functional when
		// authentication.redirect_paths is omitted from the manifest.
		redirectPaths = []string{"/auth/callback"}
	}

	seen := make(map[string]struct{})
	var uris []RedirectURI

	addURIs := func(scheme, hostsCSV, path string) {
		if hostsCSV == "" || path == "" {
			return
		}
		for _, host := range strings.Split(hostsCSV, ",") {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			u := scheme + "://" + host + path
			if _, ok := seen[u]; !ok {
				seen[u] = struct{}{}
				uris = append(uris, RedirectURI{MatchingMode: "strict", URL: u})
			}
		}
	}

	for _, path := range redirectPaths {
		addURIs("https", stageDomains.Private, path)
		addURIs("https", specDomains.Private, path)
		addURIs("https", stageDomains.Public, path)
		addURIs("https", specDomains.Public, path)
	}

	// Localhost for local dev
	for _, port := range app.Spec.Auth.LocalhostPorts {
		for _, path := range redirectPaths {
			addURIs("http", fmt.Sprintf("localhost:%d", port), path)
		}
	}

	if uris == nil {
		uris = []RedirectURI{}
	}
	return appendRegex(uris)
}

// redirectURIsEqual compares two redirect URI lists as unordered sets.
func redirectURIsEqual(a, b []RedirectURI) bool {
	if len(a) != len(b) {
		return false
	}
	toSet := func(uris []RedirectURI) []string {
		s := make([]string, len(uris))
		for i, u := range uris {
			s[i] = u.MatchingMode + "|" + u.URL
		}
		sort.Strings(s)
		return s
	}
	sa, sb := toSet(a), toSet(b)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// resolvePropertyMappings returns the property mapping UUIDs for the given scope names.
// Reads UUIDs from base.yaml to avoid a round-trip to the Authentik API.
func resolvePropertyMappings(scopes []string, base *manifest.BaseConfig) []string {
	if len(scopes) == 0 {
		scopes = base.Authentik.DefaultScopes
	}
	var pks []string
	for _, scope := range scopes {
		if pk, ok := base.Authentik.PropertyMappings[scope]; ok {
			pks = append(pks, pk)
		}
	}
	if pks == nil {
		return []string{}
	}
	return pks
}
