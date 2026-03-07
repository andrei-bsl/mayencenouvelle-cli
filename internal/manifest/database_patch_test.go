package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// minimalManifestYAML is a stripped-down manifest without secrets.inject DATABASE_URL.
const minimalManifestYAML = `apiVersion: mnlab/v1
kind: AppConfig

metadata:
  name: test-app
  description: "Test application"

spec:
  enabled: true
  type: coolify-app

  capabilities:
    auth: none
    exposure: internal
    tls: self-signed
    dns: false
    webhooks: false
    monitoring: false

  repository:
    url: https://github.com/example/test-app.git
    branch: develop

  runtime:
    port: 3000

  domains:
    private: test-app.apps.mayencenouvelle.internal

  secrets:
    vault_path: mn/data/apps/test-app
    inject:
      - env: AUTHENTIK_CLIENT_ID
        vault_key: authentik_client_id
`

// manifestWithDB is a manifest that already has DATABASE_URL in inject.
const manifestWithDB = `apiVersion: mnlab/v1
kind: AppConfig

metadata:
  name: test-app
  description: "Test application"

spec:
  enabled: true
  type: coolify-app

  capabilities:
    auth: none
    exposure: internal
    tls: self-signed
    dns: false
    webhooks: false
    monitoring: false

  repository:
    url: https://github.com/example/test-app.git
    branch: develop

  runtime:
    port: 3000

  domains:
    private: test-app.apps.mayencenouvelle.internal

  secrets:
    vault_path: mn/data/apps/test-app
    inject:
      - env: DATABASE_URL
        vault_key: DATABASE_URL
        vault_path: mn/data/lab/db01/apps/test-app
      - env: AUTHENTIK_CLIENT_ID
        vault_key: authentik_client_id
`

// TestPatchSecrets_AddsInjectEntry verifies that PatchSecrets adds a DATABASE_URL
// inject entry when one is missing.
func TestPatchSecrets_AddsInjectEntry(t *testing.T) {
	dir := t.TempDir()
	appsDir := filepath.Join(dir, "apps")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(appsDir, "test-app.yaml")
	if err := os.WriteFile(manifestPath, []byte(minimalManifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := &Loader{manifestsDir: dir, appsDir: appsDir}
	changed, err := loader.PatchSecrets("test-app", "mn/data/apps/test-app", "mn/data/lab/db01/apps/test-app")
	if err != nil {
		t.Fatalf("PatchSecrets error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true, got false")
	}

	// Re-parse patched file and assert DATABASE_URL is present.
	out, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	var app AppConfig
	if err := yaml.Unmarshal(out, &app); err != nil {
		t.Fatalf("parse patched manifest: %v", err)
	}

	found := false
	for _, si := range app.Spec.Secrets.Inject {
		if si.Env == "DATABASE_URL" && si.VaultKey == "DATABASE_URL" && si.VaultPath == "mn/data/lab/db01/apps/test-app" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DATABASE_URL inject entry not found in patched manifest; inject = %+v", app.Spec.Secrets.Inject)
	}

	// Existing entries should be preserved.
	foundOIDC := false
	for _, si := range app.Spec.Secrets.Inject {
		if si.Env == "AUTHENTIK_CLIENT_ID" {
			foundOIDC = true
			break
		}
	}
	if !foundOIDC {
		t.Error("AUTHENTIK_CLIENT_ID inject entry was removed by patch")
	}
}

// TestPatchSecrets_Idempotent verifies that calling PatchSecrets twice does not
// change the file on the second call.
func TestPatchSecrets_Idempotent(t *testing.T) {
	dir := t.TempDir()
	appsDir := filepath.Join(dir, "apps")
	os.MkdirAll(appsDir, 0o755)

	manifestPath := filepath.Join(appsDir, "test-app.yaml")
	os.WriteFile(manifestPath, []byte(manifestWithDB), 0o644)

	loader := &Loader{manifestsDir: dir, appsDir: appsDir}

	changed, err := loader.PatchSecrets("test-app", "mn/data/apps/test-app", "mn/data/lab/db01/apps/test-app")
	if err != nil {
		t.Fatalf("PatchSecrets error: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when DATABASE_URL already present, got changed=true")
	}
}

// TestPatchSecrets_SetsVaultPath verifies that PatchSecrets sets secrets.vault_path
// when it is currently empty.
func TestPatchSecrets_SetsVaultPath(t *testing.T) {
	const noVaultPath = `apiVersion: mnlab/v1
kind: AppConfig
metadata:
  name: test-app
  description: "test"
spec:
  enabled: true
  type: coolify-app
  capabilities:
    auth: none
    exposure: internal
  repository:
    url: https://github.com/example/test-app.git
    branch: develop
  runtime:
    port: 3000
  domains:
    private: test-app.apps.mayencenouvelle.internal
  secrets:
    inject:
      - env: SOME_KEY
        vault_key: some_key
`

	dir := t.TempDir()
	appsDir := filepath.Join(dir, "apps")
	os.MkdirAll(appsDir, 0o755)
	os.WriteFile(filepath.Join(appsDir, "test-app.yaml"), []byte(noVaultPath), 0o644)

	loader := &Loader{manifestsDir: dir, appsDir: appsDir}
	changed, err := loader.PatchSecrets("test-app", "mn/data/apps/test-app", "mn/data/lab/db01/apps/test-app")
	if err != nil {
		t.Fatalf("PatchSecrets error: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}

	out, _ := os.ReadFile(filepath.Join(appsDir, "test-app.yaml"))
	var app AppConfig
	yaml.Unmarshal(out, &app)

	if app.Spec.Secrets.VaultPath != "mn/data/apps/test-app" {
		t.Errorf("vault_path not set: got %q", app.Spec.Secrets.VaultPath)
	}
}

// TestDatabase_VaultPath verifies that Database struct is correctly parsed.
func TestDatabase_VaultPath(t *testing.T) {
	const manifestWithDatabase = `apiVersion: mnlab/v1
kind: AppConfig
metadata:
  name: api
  description: "test"
spec:
  enabled: true
  type: coolify-app
  capabilities:
    auth: oidc
    exposure: external
  repository:
    url: https://github.com/example/api.git
    branch: develop
  runtime:
    port: 3000
  domains:
    private: api.apps.mayencenouvelle.internal
  database:
    enabled: true
    name: public_api
    role: api
    extensions:
      - uuid-ossp
    ssl_mode: disable
`

	var app AppConfig
	if err := yaml.Unmarshal([]byte(manifestWithDatabase), &app); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !app.Spec.Database.Enabled {
		t.Error("Database.Enabled should be true")
	}
	if app.Spec.Database.Name != "public_api" {
		t.Errorf("Database.Name: got %q, want %q", app.Spec.Database.Name, "public_api")
	}
	if app.Spec.Database.Role != "api" {
		t.Errorf("Database.Role: got %q, want %q", app.Spec.Database.Role, "api")
	}
	if len(app.Spec.Database.Extensions) != 1 || app.Spec.Database.Extensions[0] != "uuid-ossp" {
		t.Errorf("Database.Extensions: got %v", app.Spec.Database.Extensions)
	}
	if app.Spec.Database.SSLMode != "disable" {
		t.Errorf("Database.SSLMode: got %q", app.Spec.Database.SSLMode)
	}
}
