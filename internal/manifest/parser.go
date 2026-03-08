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

// LoadBase reads the base.yaml global configuration.
func (l *Loader) LoadBase() (*BaseConfig, error) {
	path := filepath.Join(l.manifestsDir, "base.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading base.yaml: %w", err)
	}

	var base BaseConfig
	if err := yaml.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("parsing base.yaml: %w", err)
	}

	return &base, nil
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

// AppFilePath returns the absolute path to an app manifest file.
func (l *Loader) AppFilePath(name string) string {
	return filepath.Join(l.appsDir, name+".yaml")
}

// PatchSecrets ensures the manifest for appName has:
//  1. spec.secrets.vault_path set to vaultPath (if currently empty)
//  2. spec.environment.DATABASE_URL set to "${vault:dbVaultPath#DATABASE_URL}" (if missing)
//
// The environment block is used (not secrets.inject) so the vault reference is
// self-documenting and resolved by the standard resolveVaultRefs pass at deploy time.
// The file is edited in-place using the yaml.v3 AST, which preserves comments
// and overall structure. Returns changed=true if the file was actually modified.
func (l *Loader) PatchSecrets(appName, vaultPath, dbVaultPath string) (bool, error) {
	filePath := l.AppFilePath(appName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false, fmt.Errorf("read manifest %s: %w", filePath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse manifest %s: %w", filePath, err)
	}
	if len(doc.Content) == 0 {
		return false, fmt.Errorf("empty manifest document")
	}

	root := doc.Content[0] // root MappingNode
	specNode := findMappingValue(root, "spec")
	if specNode == nil {
		return false, fmt.Errorf("manifest has no spec block")
	}

	changed := false

	// ── Patch spec.secrets.vault_path (if provided) ──────────────────────────
	if vaultPath != "" {
		secretsNode := findMappingValue(specNode, "secrets")
		if secretsNode == nil {
			secretsNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			specNode.Content = append(specNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "secrets"},
				secretsNode,
			)
		}
		patchStringValue(secretsNode, "vault_path", vaultPath, &changed)
	}

	// ── Patch spec.environment.DATABASE_URL with vault ref ───────────────────
	// Written as ${vault:dbVaultPath#DATABASE_URL} so it is self-documenting
	// and resolved by the standard resolveVaultRefs pass at deploy time.
	if dbVaultPath != "" {
		ref := fmt.Sprintf("${vault:%s#DATABASE_URL}", dbVaultPath)
		patchEnvironmentValue(specNode, "DATABASE_URL", ref, &changed)
	}

	if !changed {
		return false, nil
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return false, fmt.Errorf("marshal patched manifest: %w", err)
	}
	if err := os.WriteFile(filePath, out, 0o644); err != nil {
		return false, fmt.Errorf("write patched manifest %s: %w", filePath, err)
	}
	return true, nil
}

// ── yaml.v3 AST helpers ───────────────────────────────────────────────────────

// findMappingValue returns the value node for key in a MappingNode, or nil.
func findMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// patchStringValue sets key=value in a MappingNode, adding the pair if absent.
// Only modifies *changed if the file actually changes.
func patchStringValue(mapping *yaml.Node, key, value string, changed *bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			// Key exists; update only if value is empty (don't overwrite intentional values).
			if mapping.Content[i+1].Value == "" && value != "" {
				mapping.Content[i+1].Value = value
				*changed = true
			}
			return
		}
	}
	// Key absent — insert before inject (if present) so vault_path appears first.
	insertBeforeKey(mapping, key, value, "inject", changed)
}

// insertBeforeKey adds key=value pair to mapping, inserted before beforeKey if found,
// or appended at the end otherwise.
func insertBeforeKey(mapping *yaml.Node, key, value, beforeKey string, changed *bool) {
	newKey := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	newVal := &yaml.Node{Kind: yaml.ScalarNode, Value: value}

	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == beforeKey {
			// Build a fresh slice to avoid aliasing the original backing array
			// (a classic Go gotcha when insert position is at the start).
			fresh := make([]*yaml.Node, 0, len(mapping.Content)+2)
			fresh = append(fresh, mapping.Content[:i]...)
			fresh = append(fresh, newKey, newVal)
			fresh = append(fresh, mapping.Content[i:]...)
			mapping.Content = fresh
			*changed = true
			return
		}
	}
	mapping.Content = append(mapping.Content, newKey, newVal)
	*changed = true
}

// patchEnvironmentValue ensures spec.environment[key] = value in a spec MappingNode.
// If the environment block does not exist, it is created. If the key already exists
// (with any value), it is not overwritten — the existing value takes precedence.
func patchEnvironmentValue(specNode *yaml.Node, key, value string, changed *bool) {
	envNode := findMappingValue(specNode, "environment")
	if envNode == nil {
		envNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		specNode.Content = append(specNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "environment"},
			envNode,
		)
	}
	// Key already present — don't overwrite.
	for i := 0; i+1 < len(envNode.Content); i += 2 {
		if envNode.Content[i].Value == key {
			return
		}
	}
	// Prepend so DATABASE_URL appears at the top of the environment block.
	fresh := make([]*yaml.Node, 0, len(envNode.Content)+2)
	fresh = append(fresh,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
	fresh = append(fresh, envNode.Content...)
	envNode.Content = fresh
	*changed = true
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
