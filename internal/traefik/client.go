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
	"regexp"
	"sort"
	"strings"

	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"gopkg.in/yaml.v3"
)

// Client manages Traefik dynamic config file generation.
type Client struct {
	configDir string // Path Traefik's file provider watches (e.g. /etc/traefik/dynamic)
}

var invalidRouterChars = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

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

// SyncManagedPublicRouters upserts public-domain routers for a Coolify app into
// a dedicated generated file (coolify-apps-public-managed.yml).
//
// This keeps manual config files untouched while making deploy idempotent.
func (c *Client) SyncManagedPublicRouters(app *manifest.AppConfig) error {
	if c.configDir == "" {
		return fmt.Errorf("traefik config dir is empty")
	}
	domains := app.GetDomains()
	if domains.External == "" {
		return nil // nothing to manage
	}

	cfg, err := c.loadManagedPublicConfig()
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	if cfg.HTTP.Routers == nil {
		cfg.HTTP.Routers = make(map[string]router)
	}

	prefix := managedRouterPrefix(app)
	for key := range cfg.HTTP.Routers {
		if key == prefix+"-public" || strings.HasPrefix(key, prefix+"-public-") {
			delete(cfg.HTTP.Routers, key)
		}
	}

	hosts := splitDomains(domains.External)
	for i, host := range hosts {
		name := prefix + "-public"
		if i > 0 {
			name = fmt.Sprintf("%s-public-%d", prefix, i+1)
		}
		cfg.HTTP.Routers[name] = router{
			Rule:        buildHostRule(host),
			Service:     "coolify-traefik-svc@file",
			Entrypoints: []string{"websecure"},
			TLS:         &tlsCfg{CertResolver: "letsencrypt"},
		}
	}

	return c.writeManagedPublicConfig(cfg)
}

// RemoveManagedPublicRouters deletes all managed public routers for an app+stage.
func (c *Client) RemoveManagedPublicRouters(app *manifest.AppConfig) error {
	if c.configDir == "" {
		return fmt.Errorf("traefik config dir is empty")
	}
	cfg, err := c.loadManagedPublicConfig()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if cfg.HTTP.Routers == nil {
		return nil
	}

	prefix := managedRouterPrefix(app)
	changed := false
	for key := range cfg.HTTP.Routers {
		if key == prefix+"-public" || strings.HasPrefix(key, prefix+"-public-") {
			delete(cfg.HTTP.Routers, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.writeManagedPublicConfig(cfg)
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

func splitDomains(domains string) []string {
	parts := strings.Split(domains, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func managedRouterPrefix(app *manifest.AppConfig) string {
	name := app.Metadata.Name
	stage := app.GetEnvironmentStage()
	if stage == "development" || stage == "development-internal" {
		name = "dev-" + name
	}
	return sanitizeRouterName(name)
}

func sanitizeRouterName(name string) string {
	name = invalidRouterChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "app"
	}
	return name
}

func (c *Client) managedPublicPath() string {
	return filepath.Join(c.configDir, "coolify-apps-public-managed.yml")
}

func (c *Client) loadManagedPublicConfig() (dynamicConfig, error) {
	path := c.managedPublicPath()
	var cfg dynamicConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing managed traefik config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Client) writeManagedPublicConfig(cfg dynamicConfig) error {
	path := c.managedPublicPath()
	if len(cfg.HTTP.Routers) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing managed traefik file %s: %w", path, err)
		}
		return nil
	}
	if cfg.HTTP.Routers == nil {
		cfg.HTTP.Routers = make(map[string]router)
	}
	keys := make([]string, 0, len(cfg.HTTP.Routers))
	for k := range cfg.HTTP.Routers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]router, len(keys))
	for _, k := range keys {
		ordered[k] = cfg.HTTP.Routers[k]
	}
	cfg.HTTP.Routers = ordered
	cfg.HTTP.Services = nil
	cfg.HTTP.Middlewares = nil

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling managed traefik config: %w", err)
	}
	if err := os.MkdirAll(c.configDir, 0755); err != nil {
		return fmt.Errorf("ensuring traefik config dir %s: %w", c.configDir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing managed traefik temp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming managed traefik file %s: %w", path, err)
	}
	return nil
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
