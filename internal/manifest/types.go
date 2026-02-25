// Package manifest provides types and loading for mnlab/v1 app manifests.
package manifest

import "strings"

// AppConfig represents a parsed app manifest (apiVersion: mnlab/v1, kind: AppConfig).
type AppConfig struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// BaseConfig represents the base.yaml global configuration.
// It contains platform-wide settings inherited by all app manifests.
type BaseConfig struct {
	Version      string        `yaml:"version"`
	Updated      string        `yaml:"updated"`
	Organization string        `yaml:"organization"`
	LabName      string        `yaml:"lab_name"`
	Coolify      CoolifyBase   `yaml:"coolify"`
	Authentik    AuthentikBase `yaml:"authentik"`
	Traefik      TraefikBase   `yaml:"traefik"`
	DNS          DNSBase       `yaml:"dns"`
	GitHub       GitHubBase    `yaml:"github"`
}

// CoolifyBase holds Coolify platform configuration.
type CoolifyBase struct {
	Project         string `yaml:"project"`
	ProjectUUID     string `yaml:"project_uuid"`
	Environment     string `yaml:"environment"`
	Endpoint        string `yaml:"endpoint"`
	ServerUUID      string `yaml:"server_uuid"`
	DestinationUUID string `yaml:"destination_uuid"`
	// GitHubAppUUID is the Coolify GitHub App source UUID.
	// Using a GitHub App grants scoped access to all org repos via a single
	// installation — no per-repo SSH deploy keys needed.
	// Register once in Coolify → Sources → + Add.
	GitHubAppUUID string `yaml:"github_app_uuid"`
	// PrivateKeyUUID is the legacy Coolify SSH deploy key UUID.
	// Deprecated: use github_app_uuid instead. Kept as fallback for repos
	// not covered by the GitHub App installation.
	PrivateKeyUUID string `yaml:"private_key_uuid"`
}

// AuthentikBase holds Authentik platform configuration.
type AuthentikBase struct {
	Endpoint          string            `yaml:"endpoint"`
	AdminPath         string            `yaml:"admin_path"`
	DefaultScopes     []string          `yaml:"default_scopes"`
	AuthorizationFlow string            `yaml:"authorization_flow"`
	InvalidationFlow  string            `yaml:"invalidation_flow"`
	PropertyMappings  map[string]string `yaml:"property_mappings"`
	// InternalAltDomainSuffix is the alternate external domain suffix for internal apps.
	// E.g. "internal.apps.mayencenouvelle.com" causes
	// "vpn.apps.mayencenouvelle.internal" → also add "vpn.internal.apps.mayencenouvelle.com".
	// Empty string disables this expansion.
	InternalAltDomainSuffix string `yaml:"internal_alt_domain_suffix"`
}

// TraefikBase holds Traefik platform configuration.
type TraefikBase struct {
	AdminEndpoint string            `yaml:"admin_endpoint"`
	Entrypoints   map[string]string `yaml:"entrypoints"`
	InternalIP    string            `yaml:"internal_ip"`
}

// DNSBase holds DNS platform configuration.
type DNSBase struct {
	Provider       string `yaml:"provider"`
	Endpoint       string `yaml:"endpoint"`
	InternalTarget string `yaml:"internal_target"`
}

// GitHubBase holds GitHub platform configuration.
type GitHubBase struct {
	Organization string `yaml:"organization"`
}

// Metadata holds identifying information about the app.
type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Team        string `yaml:"team"`
	Phase       string `yaml:"phase"`
	// Category determines the vault path prefix for secret storage:
	// mn/{category}/{app-name}. Valid values: apps, integrations, infra,
	// iot, platform, cicd, experimental, external, legacy. Default: apps.
	Category string            `yaml:"category,omitempty"`
	Labels   map[string]string `yaml:"labels"`
}

// Spec holds the full deployment specification.
type Spec struct {
	Enabled              bool           `yaml:"enabled"`
	Type                 string         `yaml:"type"` // coolify-app | systemd-service | scheduled-job | external
	Capabilities         Capabilities   `yaml:"capabilities"`
	Repository           Repository     `yaml:"repository"`
	Build                Build          `yaml:"build"`
	Runtime              Runtime        `yaml:"runtime"`
	Domains              Domains        `yaml:"domains"`
	Environment          Env            `yaml:"environment"`
	EnvironmentOverrides map[string]Env `yaml:"environment_overrides,omitempty"`
	Secrets              Secrets        `yaml:"secrets"`
	Auth                 Auth           `yaml:"authentication"`
	Traefik              TraefikSpec    `yaml:"traefik"`
	Dependencies         []string       `yaml:"dependencies"`
	Node                 string         `yaml:"node"`
	Schedule             string         `yaml:"schedule"`
	Note                 string         `yaml:"note"`
}

// ApplyStageOverrides merges environment_overrides[stage] into the base
// environment map. Only keys present in the override are replaced;
// all other base keys are preserved.
func (a *AppConfig) ApplyStageOverrides(stage string) {
	overrides, ok := a.Spec.EnvironmentOverrides[stage]
	if !ok {
		return
	}
	if a.Spec.Environment == nil {
		a.Spec.Environment = make(Env)
	}
	for k, v := range overrides {
		a.Spec.Environment[k] = v
	}
}

// Secrets defines OpenBao vault integration for this app.
// vault_path is the default KV v2 path; inject maps vault keys to env vars.
type Secrets struct {
	VaultPath string         `yaml:"vault_path,omitempty"` // e.g. mn/data/apps/internal-api
	Inject    []SecretInject `yaml:"inject,omitempty"`
}

// SecretInject maps a vault key to a Coolify environment variable.
type SecretInject struct {
	Env       string `yaml:"env"`                  // env var name, e.g. AUTHENTIK_CLIENT_ID
	VaultKey  string `yaml:"vault_key"`            // key within the vault secret, e.g. client_id
	VaultPath string `yaml:"vault_path,omitempty"` // override path for cross-app refs
}

// Capabilities declares which platform features to activate.
type Capabilities struct {
	Auth       string `yaml:"auth"`     // oidc | forwardauth | jwt | none
	Exposure   string `yaml:"exposure"` // internal | external | both
	TLS        string `yaml:"tls"`      // letsencrypt | self-signed | none
	DNS        bool   `yaml:"dns"`
	Webhooks   bool   `yaml:"webhooks"`
	Monitoring bool   `yaml:"monitoring"`
}

// Repository holds source code location.
type Repository struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
	// PrivateKeyUUID is a legacy per-repo SSH deploy key override.
	// Deprecated: GitHub App (base.yaml github_app_uuid) replaces per-repo keys.
	// Kept only for backward compatibility with existing Coolify resources.
	PrivateKeyUUID string `yaml:"private_key_uuid,omitempty"`
}

// Build holds Coolify build parameters.
type Build struct {
	Command    string `yaml:"command"`
	Workdir    string `yaml:"workdir"`
	BaseImage  string `yaml:"base_image"`
	Dockerfile string `yaml:"dockerfile"`
}

// Runtime holds container/service runtime parameters.
type Runtime struct {
	Port           int    `yaml:"port"`
	StartCommand   string `yaml:"start_command"`
	MemoryLimit    string `yaml:"memory_limit"`
	CPULimit       string `yaml:"cpu_limit"`
	HealthEndpoint string `yaml:"health_endpoint"`
	HealthInterval string `yaml:"health_interval"`
}

// Domains holds the hostname assignments per zone.
type Domains struct {
	Private string `yaml:"private"`
	Public  string `yaml:"public"`
	// Legacy keys (deprecated): kept for backward compatibility during migration.
	Internal string `yaml:"internal,omitempty"`
	External string `yaml:"external,omitempty"`
}

// Env is a map of environment variable name → value (may contain ${env:X} refs).
type Env map[string]string

// Auth holds OIDC/OAuth2 configuration.
type Auth struct {
	// ClientType is the OAuth2 client type: "public" (PKCE, for SPAs) or
	// "confidential" (client secret, for server-side apps). Default: "public".
	ClientType    string   `yaml:"client_type"`
	Scopes        []string `yaml:"scopes"`
	RedirectPaths []string `yaml:"redirect_paths"`
	LogoutURL     string   `yaml:"logout_url"`
	AllowedGroups []string `yaml:"allowed_groups"`
	// LibraryGroup overrides the Authentik application library group shown on the
	// user dashboard. Defaults to "Internal" for exposure=internal and "Apps" for
	// exposure=external|both. Use this when the auto-derived group doesn't match
	// — e.g. an external test app that should appear under "Test" instead of "Apps".
	LibraryGroup string `yaml:"library_group,omitempty"`
	// LocalhostPorts lists local dev server ports to include as redirect URI
	// origins (http://localhost:{port}{redirect_path}). Useful for local dev.
	LocalhostPorts []int `yaml:"localhost_ports"`
	// RedirectURIs is an optional explicit list of full redirect URIs.
	// When set, these are sent to Authentik verbatim (all strict matching) and
	// the auto-derivation from domains + redirect_paths is skipped entirely.
	// Use this when the auto-generated URIs do not match your requirements.
	// Example:
	//   redirect_uris:
	//     - https://hello-world.apps.mayencenouvelle.internal/auth/callback
	//     - https://custom.example.com/callback
	RedirectURIs []string `yaml:"redirect_uris"`
}

// TraefikSpec holds app-specific Traefik overrides.
type TraefikSpec struct {
	Middlewares []string          `yaml:"middlewares"`
	ExtraLabels map[string]string `yaml:"extra_labels"`
}

// EffectiveVaultPath returns the vault_path from secrets block,
// falling back to the auto-derived VaultPath() if not explicitly set.
func (a *AppConfig) EffectiveVaultPath() string {
	if a.Spec.Secrets.VaultPath != "" {
		return a.Spec.Secrets.VaultPath
	}
	return a.VaultPath()
}

// AppSlug returns the Authentik application slug: "mn-{name}".
// Slugs are unique, kebab-case identifiers used for API lookup.
func (a *AppConfig) AppSlug() string {
	return "mn-" + a.Metadata.Name
}

// VaultCategory returns the vault path category for this app.
// Defaults to "apps" if not specified in the manifest.
func (a *AppConfig) VaultCategory() string {
	if a.Metadata.Category != "" {
		return a.Metadata.Category
	}
	return "apps"
}

// VaultPath returns the full KV v2 secret path for this app.
// Format: mn/data/{category}/{app-name}  (API path for KV v2)
// Example: mn/data/apps/hello-world
func (a *AppConfig) VaultPath() string {
	return "mn/data/" + a.VaultCategory() + "/" + a.Metadata.Name
}

// ProviderName returns the Authentik OAuth2 provider name: "mn-{name}-provider".
func (a *AppConfig) ProviderName() string {
	return "mn-" + a.Metadata.Name + "-provider"
}

// AppDisplayName returns a human-readable display name for the Authentik application.
// Derived by title-casing the app's metadata name (e.g. "vpn-app" → "Vpn App").
// Authentik administrators can update the display name in the UI without affecting
// the slug or provider lookup which are keyed on AppSlug() / ProviderName().
func (a *AppConfig) AppDisplayName() string {
	parts := strings.Split(a.Metadata.Name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// AuthentikGroup returns the Authentik application library group for this app.
// This determines how the app appears in the Authentik user dashboard.
//
// Resolution order:
//  1. authentication.library_group in the manifest (explicit override)
//  2. Derived from capabilities.exposure:
//     "internal" → "Internal" (admin and ops tools)
//     "external" / "both" → "Apps" (end-user facing)
func (a *AppConfig) AuthentikGroup() string {
	if a.Spec.Auth.LibraryGroup != "" {
		return a.Spec.Auth.LibraryGroup
	}
	switch a.Spec.Capabilities.Exposure {
	case "external", "both":
		return "Apps"
	default:
		return "Internal"
	}
}

// OAuth2ClientType returns the OAuth2 client type for Authentik provider creation.
// Defaults to "public" (PKCE for SPAs) if not specified in the manifest.
func (a *AppConfig) OAuth2ClientType() string {
	if a.Spec.Auth.ClientType != "" {
		return a.Spec.Auth.ClientType
	}
	return "public"
}

// Validate runs structural validation on the parsed manifest.
// Returns a list of error strings; empty means valid.
func (a *AppConfig) Validate() []string {
	var errs []string

	if a.APIVersion != "mnlab/v1" {
		errs = append(errs, "apiVersion must be 'mnlab/v1'")
	}
	if a.Kind != "AppConfig" {
		errs = append(errs, "kind must be 'AppConfig'")
	}
	if a.Metadata.Name == "" {
		errs = append(errs, "metadata.name is required")
	}
	if a.Spec.Runtime.Port == 0 {
		errs = append(errs, "spec.runtime.port is required")
	}

	// Domain checks (new model): Coolify apps must expose at least one domain.
	// Non-ingress workloads (e.g. systemd-service, scheduled-job) may not have domains.
	if a.Spec.Type == "" || a.Spec.Type == "coolify-app" {
		domains := a.NormalizedDomains()
		if domains.Private == "" && domains.Public == "" {
			errs = append(errs, "at least one of domains.private or domains.public is required")
		}
	}

	// Type-specific checks
	if a.Spec.Type == "systemd-service" && a.Spec.Node == "" {
		errs = append(errs, "spec.node is required for systemd-service type")
	}
	if a.Spec.Type == "scheduled-job" && a.Spec.Schedule == "" {
		errs = append(errs, "spec.schedule is required for scheduled-job type")
	}

	return errs
}

// GetEnvironmentStage returns the Coolify environment name based on branch only.
// Maps:
//   - develop/* -> "development"
//   - main/master -> "production"
func (a *AppConfig) GetEnvironmentStage() string {
	branch := a.Spec.Repository.Branch

	isProduction := branch == "main" || branch == "master"
	if isProduction {
		return "production"
	}
	return "development"
}

// NormalizedDomains resolves legacy domain keys into the new private/public model.
// Priority: explicit private/public keys, then fallback to internal/external keys.
func (a *AppConfig) NormalizedDomains() Domains {
	d := a.Spec.Domains
	if d.Private == "" && d.Internal != "" {
		d.Private = d.Internal
	}
	if d.Public == "" && d.External != "" {
		d.Public = d.External
	}
	return d
}

// GetDomains returns the actual domains used for deployment, applying stage-based prefixing.
// For development environments, prepends "dev-" to domain names (per ADR-016).
// Example: internal app on develop branch: "hello-world.apps.mayencenouvelle.internal"
//
//	becomes "dev-hello-world.apps.mayencenouvelle.internal"
func (a *AppConfig) GetDomains() Domains {
	domains := a.NormalizedDomains()
	stage := a.GetEnvironmentStage()

	// Development-stage apps get "dev-" prefix
	isDevelopment := stage == "development"
	if isDevelopment {
		if domains.Private != "" {
			domains.Private = prefixDomains(domains.Private, "dev-")
		}
		if domains.Public != "" {
			domains.Public = prefixDomains(domains.Public, "dev-")
		}
	}

	return domains
}

// prefixDomains adds a prefix to each domain in a comma-separated list.
// Each domain receives the prefix before its first label (before the first dot).
// Example: prefixDomains("a.example.internal,b.example.com", "dev-")
//
//	→ "dev-a.example.internal,dev-b.example.com"
func prefixDomains(domains, prefix string) string {
	parts := strings.Split(domains, ",")
	for i, p := range parts {
		parts[i] = prefixDomain(strings.TrimSpace(p), prefix)
	}
	return strings.Join(parts, ",")
}

// prefixDomain adds a prefix to a single domain name (before the first dot).
// Example: prefixDomain("hello.apps.mayencenouvelle.internal", "dev-")
//
//	→ "dev-hello.apps.mayencenouvelle.internal"
func prefixDomain(domain, prefix string) string {
	for i, ch := range domain {
		if ch == '.' {
			return prefix + domain[:i] + domain[i:]
		}
	}
	return prefix + domain
}
