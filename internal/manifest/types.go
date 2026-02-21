// Package manifest provides types and loading for mnlab/v1 app manifests.
package manifest

// AppConfig represents a parsed app manifest (apiVersion: mnlab/v1, kind: AppConfig).
type AppConfig struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
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
