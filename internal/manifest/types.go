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
	Version      string       `yaml:"version"`
	Updated      string       `yaml:"updated"`
	Organization string       `yaml:"organization"`
	LabName      string       `yaml:"lab_name"`
	Coolify      CoolifyBase  `yaml:"coolify"`
	Authentik    AuthentikBase `yaml:"authentik"`
	Traefik      TraefikBase  `yaml:"traefik"`
	DNS          DNSBase      `yaml:"dns"`
	GitHub       GitHubBase   `yaml:"github"`
}

// CoolifyBase holds Coolify platform configuration.
type CoolifyBase struct {
	Project         string `yaml:"project"`
	ProjectUUID     string `yaml:"project_uuid"`
	Environment     string `yaml:"environment"`
	Endpoint        string `yaml:"endpoint"`
	ServerUUID      string `yaml:"server_uuid"`
	DestinationUUID string `yaml:"destination_uuid"`
	// PrivateKeyUUID is the Coolify SSH deploy key UUID (from Keys & Certificates).
	// Used when creating applications from private GitHub repositories.
	// Corresponds to the 'github-deploy' key registered in Coolify.
	PrivateKeyUUID  string `yaml:"private_key_uuid"`
}

// AuthentikBase holds Authentik platform configuration.
type AuthentikBase struct {
	Endpoint               string            `yaml:"endpoint"`
	AdminPath              string            `yaml:"admin_path"`
	DefaultScopes          []string          `yaml:"default_scopes"`
	AuthorizationFlow      string            `yaml:"authorization_flow"`
	InvalidationFlow       string            `yaml:"invalidation_flow"`
	PropertyMappings       map[string]string `yaml:"property_mappings"`
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
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Team        string            `yaml:"team"`
	Phase       string            `yaml:"phase"`
	// Category determines the vault path prefix for secret storage:
	// mn/{category}/{app-name}. Valid values: apps, integrations, infra,
	// iot, platform, cicd, experimental, external, legacy. Default: apps.
	Category    string            `yaml:"category,omitempty"`
	Labels      map[string]string `yaml:"labels"`
}

// Spec holds the full deployment specification.
type Spec struct {
	Enabled      bool         `yaml:"enabled"`
	Type         string       `yaml:"type"` // coolify-app | systemd-service | scheduled-job | external
	Capabilities Capabilities `yaml:"capabilities"`
	Repository   Repository   `yaml:"repository"`
	Build        Build        `yaml:"build"`
	Runtime      Runtime      `yaml:"runtime"`
	Domains      Domains      `yaml:"domains"`
	Environment  Env          `yaml:"environment"`
	Auth         Auth         `yaml:"authentication"`
	Traefik      TraefikSpec  `yaml:"traefik"`
	Dependencies []string     `yaml:"dependencies"`
	Node         string       `yaml:"node"`
	Schedule     string       `yaml:"schedule"`
	Note         string       `yaml:"note"`
}

// Capabilities declares which platform features to activate.
type Capabilities struct {
	Auth       string `yaml:"auth"`       // oidc | forwardauth | jwt | none
	Exposure   string `yaml:"exposure"`   // internal | external | both
	TLS        string `yaml:"tls"`        // letsencrypt | self-signed | none
	DNS        bool   `yaml:"dns"`
	Webhooks   bool   `yaml:"webhooks"`
	Monitoring bool   `yaml:"monitoring"`
}

// Repository holds source code location.
type Repository struct {
	URL            string `yaml:"url"`
	Branch         string `yaml:"branch"`
	// PrivateKeyUUID overrides the global Coolify deploy key for this app.
	// Required for private repos when the global key is already used elsewhere
	// (GitHub deploy keys must be unique per repository).
	// Find UUIDs in Coolify → Keys & Certificates, or via mn-cli.
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
	Internal string `yaml:"internal"`
	External string `yaml:"external"`
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
	LibraryGroup  string   `yaml:"library_group,omitempty"`
	// LocalhostPorts lists local dev server ports to include as redirect URI
	// origins (http://localhost:{port}{redirect_path}). Useful for local dev.
	LocalhostPorts []int   `yaml:"localhost_ports"`
	// RedirectURIs is an optional explicit list of full redirect URIs.
	// When set, these are sent to Authentik verbatim (all strict matching) and
	// the auto-derivation from domains + redirect_paths is skipped entirely.
	// Use this when the auto-generated URIs do not match your requirements.
	// Example:
	//   redirect_uris:
	//     - https://hello-world.apps.mayencenouvelle.internal/auth/callback
	//     - https://custom.example.com/callback
	RedirectURIs  []string `yaml:"redirect_uris"`
}

// TraefikSpec holds app-specific Traefik overrides.
type TraefikSpec struct {
	Middlewares []string          `yaml:"middlewares"`
	ExtraLabels map[string]string `yaml:"extra_labels"`
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

	// Exposure vs domains cross-check
	switch a.Spec.Capabilities.Exposure {
	case "internal":
		if a.Spec.Domains.Internal == "" {
			errs = append(errs, "domains.internal is required when exposure=internal")
		}
	case "external":
		if a.Spec.Domains.External == "" {
			errs = append(errs, "domains.external is required when exposure=external")
		}
	case "both":
		if a.Spec.Domains.Internal == "" {
			errs = append(errs, "domains.internal is required when exposure=both")
		}
		if a.Spec.Domains.External == "" {
			errs = append(errs, "domains.external is required when exposure=both")
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
// GetEnvironmentStage returns the Coolify environment name based on branch + exposure.
// Maps:
//   - develop + internal -> "development-internal"
//   - develop + external/both -> "development"
//   - main/master + internal -> "production-internal"
//   - main/master + external/both -> "production"
func (a *AppConfig) GetEnvironmentStage() string {
	branch := a.Spec.Repository.Branch
	exposure := a.Spec.Capabilities.Exposure

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

// GetDomains returns the actual domains used for deployment, applying stage-based prefixing.
// For development environments, prepends "dev-" to domain names (per ADR-016).
// Example: internal app on develop branch: "hello-world.apps.mayencenouvelle.internal"
//          becomes "dev-hello-world.apps.mayencenouvelle.internal"
func (a *AppConfig) GetDomains() Domains {
	domains := a.Spec.Domains
	stage := a.GetEnvironmentStage()

	// Development-stage apps get "dev-" prefix
	isDevelopment := stage == "development" || stage == "development-internal"
	if isDevelopment {
		if domains.Internal != "" {
			domains.Internal = prefixDomains(domains.Internal, "dev-")
		}
		if domains.External != "" {
			domains.External = prefixDomains(domains.External, "dev-")
		}
	}

	return domains
}

// prefixDomains adds a prefix to each domain in a comma-separated list.
// Each domain receives the prefix before its first label (before the first dot).
// Example: prefixDomains("a.example.internal,b.example.com", "dev-")
//   → "dev-a.example.internal,dev-b.example.com"
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