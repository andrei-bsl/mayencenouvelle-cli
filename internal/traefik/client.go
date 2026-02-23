// Package traefik generates and manages Traefik file-provider dynamic config
// for app routes, TLS settings, and middleware chains.
//
// Design principles:
//   - Generated YAML files are idempotent (overwrite-safe)
//   - One file per app: <config-dir>/<app-name>.yaml
//   - Traefik file-provider auto-detects changes (no reload needed)
//   - Dual-zone apps get two separate routers (different TLS + middleware chains)
package traefik

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
)

// Client manages Traefik dynamic config file generation.
type Client struct {
	configDir string // Path Traefik's file provider watches (e.g. /etc/traefik/dynamic)
}

// NewClient creates a Traefik client.
//
//	configDir: directory Traefik watches for dynamic config files
func NewClient(configDir string) *Client {
	return &Client{configDir: configDir}
}

// ─── Types (mirrors Traefik file-provider YAML structure) ────────────────────

type dynamicConfig struct {
	HTTP httpConfig `yaml:"http"`
}

type httpConfig struct {
	Routers     map[string]router     `yaml:"routers,omitempty"`
	Services    map[string]service    `yaml:"services,omitempty"`
	Middlewares map[string]middleware `yaml:"middlewares,omitempty"`
}

type router struct {
	Rule        string   `yaml:"rule"`
	Service     string   `yaml:"service"`
	Entrypoints []string `yaml:"entryPoints"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	TLS         *tlsCfg  `yaml:"tls,omitempty"`
}

type tlsCfg struct {
	CertResolver string `yaml:"certResolver,omitempty"`
}

type service struct {
	LoadBalancer lbConfig `yaml:"loadBalancer"`
}

type lbConfig struct {
	Servers []server `yaml:"servers"`
}

type server struct {
	URL string `yaml:"url"`
}

type middleware struct {
	// Intentionally flexible — middlewares are defined in base config
	// and referenced by name. App-specific middlewares not generated here.
}

// PlanAction is a planned change for dry-run output.
type PlanAction struct {
	Operation string
	Resource  string
	Detail    string
}

// ─── Interface ────────────────────────────────────────────────────────────────

// ApplyRoutes generates and writes the Traefik dynamic config YAML for an app.
// Idempotent: overwrites existing file if present.
func (c *Client) ApplyRoutes(app *manifest.AppConfig) error {
	cfg := c.buildConfig(app)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling traefik config: %w", err)
	}

	path := filepath.Join(c.configDir, app.Metadata.Name+".yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing traefik config to %s: %w", path, err)
	}
	return nil
}

// RemoveRoutes deletes the Traefik dynamic config file for an app.
func (c *Client) RemoveRoutes(app *manifest.AppConfig) error {
	path := filepath.Join(c.configDir, app.Metadata.Name+".yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing traefik config: %w", err)
	}
	return nil
}

// PlanRoutes returns a dry-run preview of what ApplyRoutes would write.
func (c *Client) PlanRoutes(app *manifest.AppConfig) ([]PlanAction, error) {
	path := filepath.Join(c.configDir, app.Metadata.Name+".yaml")
	_, err := os.Stat(path)
	exists := !os.IsNotExist(err)

	op := "create"
	if exists {
		op = "update"
	}

	// Use GetDomains() to apply stage-based domain transformations (e.g., dev- prefix)
	domains := app.GetDomains()

	var actions []PlanAction
	if domains.Internal != "" {
		actions = append(actions, PlanAction{
			Operation: op,
			Resource:  "Traefik Router (internal)",
			Detail:    buildHostRule(domains.Internal),
		})
	}
	if domains.External != "" {
		actions = append(actions, PlanAction{
			Operation: op,
			Resource:  "Traefik Router (external)",
			Detail:    buildHostRule(domains.External) + " + Let's Encrypt TLS",
		})
	}
	return actions, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// buildHostRule converts a comma-separated list of hostnames into a valid
// Traefik Host() rule. Single hostname: Host(`a.example.com`).
// Multiple hostnames: Host(`a.example.com`) || Host(`b.example.com`).
func buildHostRule(domains string) string {
	parts := strings.Split(domains, ",")
	rules := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			rules = append(rules, fmt.Sprintf("Host(`%s`)", p))
		}
	}
	return strings.Join(rules, " || ")
}

func (c *Client) buildConfig(app *manifest.AppConfig) dynamicConfig {
	routers := make(map[string]router)
	services := make(map[string]service)

	// The app's backend service (same for both routers if dual-zone)
	svcName := app.Metadata.Name
	services[svcName] = service{
		LoadBalancer: lbConfig{
			Servers: []server{
				{URL: fmt.Sprintf("http://localhost:%d", app.Spec.Runtime.Port)},
			},
		},
	}

	middlewares := app.Spec.Traefik.Middlewares
	if len(middlewares) == 0 {
		middlewares = []string{"compress"} // safe default
	}

	// Use GetDomains() to apply stage-based domain transformations (e.g., dev- prefix)
	domains := app.GetDomains()

	// Internal router
	if domains.Internal != "" &&
		(app.Spec.Capabilities.Exposure == "internal" || app.Spec.Capabilities.Exposure == "both") {

		certResolver := ""
		if app.Spec.Capabilities.TLS == "letsencrypt" {
			certResolver = "letsencrypt"
		}

		r := router{
			Rule:        buildHostRule(domains.Internal),
			Service:     svcName,
			Entrypoints: []string{"websecure"},
			Middlewares: middlewares,
		}
		if app.Spec.Capabilities.TLS != "none" {
			r.TLS = &tlsCfg{CertResolver: certResolver}
		}
		routers[app.Metadata.Name+"-internal"] = r
	}

	// External router — ALWAYS uses Let's Encrypt and may have different middleware
	if domains.External != "" &&
		(app.Spec.Capabilities.Exposure == "external" || app.Spec.Capabilities.Exposure == "both") {

		routers[app.Metadata.Name+"-external"] = router{
			Rule:        buildHostRule(domains.External),
			Service:     svcName,
			Entrypoints: []string{"websecure"},
			Middlewares: middlewares,
			TLS:         &tlsCfg{CertResolver: "letsencrypt"},
		}
	}

	return dynamicConfig{
		HTTP: httpConfig{
			Routers:  routers,
			Services: services,
		},
	}
}
