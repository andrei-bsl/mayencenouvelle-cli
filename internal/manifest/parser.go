package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Loader reads and parses manifests from disk.
type Loader struct {
	manifestsDir string
	appsDir      string
}

// NewLoader creates a Loader rooted at manifestsDir.
func NewLoader(manifestsDir string) (*Loader, error) {
	appsDir := filepath.Join(manifestsDir, "apps")
	if _, err := os.Stat(appsDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("manifests apps directory not found: %s", appsDir)
	}
	return &Loader{manifestsDir: manifestsDir, appsDir: appsDir}, nil
}

// LoadApp reads a single app manifest by name.
func (l *Loader) LoadApp(name string) (*AppConfig, error) {
	path := filepath.Join(l.appsDir, name+".yaml")
	return l.loadFile(path)
}

// LoadAll reads all .yaml files in the apps directory.
func (l *Loader) LoadAll() ([]*AppConfig, error) {
	entries, err := os.ReadDir(l.appsDir)
	if err != nil {
		return nil, fmt.Errorf("reading apps dir: %w", err)
	}

	var apps []*AppConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		app, err := l.loadFile(filepath.Join(l.appsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// LoadOrdered loads all apps sorted by dependency order (topological sort).
func (l *Loader) LoadOrdered() ([]*AppConfig, error) {
	apps, err := l.LoadAll()
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*AppConfig, len(apps))
	for _, a := range apps {
		byName[a.Metadata.Name] = a
	}

	visited := make(map[string]bool)
	var ordered []*AppConfig

	var visit func(name string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		visited[name] = true
		app, ok := byName[name]
		if !ok {
			return nil // external dependency, skip
		}
		for _, dep := range app.Spec.Dependencies {
			if err := visit(dep); err != nil {
				return err
			}
		}
		ordered = append(ordered, app)
		return nil
	}

	// Sort names for deterministic output
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}

	return ordered, nil
}

// loadFile parses a single YAML manifest file.
func (l *Loader) loadFile(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var app AppConfig
	if err := yaml.Unmarshal(data, &app); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	// Apply defaults
	if app.Spec.Capabilities.Auth == "" {
		app.Spec.Capabilities.Auth = "oidc"
	}
	if app.Spec.Capabilities.Exposure == "" {
		app.Spec.Capabilities.Exposure = "internal"
	}
	if app.Spec.Runtime.HealthEndpoint == "" {
		app.Spec.Runtime.HealthEndpoint = "/health"
	}
	if app.Spec.Runtime.HealthInterval == "" {
		app.Spec.Runtime.HealthInterval = "30s"
	}
	if app.Spec.Enabled == false && app.APIVersion != "" {
		// Only force true if field was omitted (zero-value false)
		// Real false from yaml will be preserved
	}

	return &app, nil
}
