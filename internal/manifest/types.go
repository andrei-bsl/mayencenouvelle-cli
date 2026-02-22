// Package manifest provides types and loading for mnlab/v1 app manifests.
package manifest

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
	Project        string `yaml:"project"`
	ProjectUUID    string `yaml:"project_uuid"`
	Environment    string `yaml:"environment"`
	Endpoint       string `yaml:"endpoint"`
	ServerUUID     string `yaml:"server_uuid"`
	DestinationUUID string `yaml:"destination_uuid"`
}

// AuthentikBase holds Authentik platform configuration.
type AuthentikBase struct {
	Endpoint     string   `yaml:"endpoint"`
	AdminPath    string   `yaml:"admin_path"`
	DefaultScopes []string `yaml:"default_scopes"`
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
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
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
	Scopes        []string `yaml:"scopes"`
	RedirectPaths []string `yaml:"redirect_paths"`
	LogoutURL     string   `yaml:"logout_url"`
	AllowedGroups []string `yaml:"allowed_groups"`
}

// TraefikSpec holds app-specific Traefik overrides.
type TraefikSpec struct {
	Middlewares []string          `yaml:"middlewares"`
	ExtraLabels map[string]string `yaml:"extra_labels"`
}

// ProviderName returns the generated Authentik OAuth2 provider name.
func (a *AppConfig) ProviderName() string {
	return a.Metadata.Name + "-oauth2"
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
			domains.Internal = prefixDomain(domains.Internal, "dev-")
		}
		if domains.External != "" {
			domains.External = prefixDomain(domains.External, "dev-")
		}
	}

	return domains
}

// prefixDomain adds a prefix to a domain name (before the first dot).
// Example: prefixDomain("hello.apps.mayencenouvelle.internal", "dev-") -> "dev-hello.apps.mayencenouvelle.internal"
func prefixDomain(domain, prefix string) string {
	// Find the first dot
	for i, ch := range domain {
		if ch == '.' {
			return prefix + domain[:i] + domain[i:]
		}
	}
	// No dot found, just prefix the whole thing
	return prefix + domain
}