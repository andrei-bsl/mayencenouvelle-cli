package manifest

import (
	"testing"
)

// TestLoadApp tests loading a single manifest file.
func TestLoadApp(t *testing.T) {
	loader, err := NewLoader("../../workspace/manifests")
	if err != nil {
		t.Fatalf("NewLoader failed: %v", err)
	}

	tests := []struct {
		name    string
		appName string
		wantErr bool
	}{
		{"nas-app exists", "nas-app", false},
		{"vpn-app exists", "vpn-app", false},
		{"internal-api exists", "internal-api", false},
		{"nonexistent app", "does-not-exist", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := loader.LoadApp(tt.appName)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadApp() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && app == nil {
				t.Error("LoadApp() returned nil app")
			}
			if !tt.wantErr && app.Metadata.Name != tt.appName {
				t.Errorf("LoadApp() name = %s, want %s", app.Metadata.Name, tt.appName)
			}
		})
	}
}

// TestLoadAll loads all manifests and verifies basic structure.
func TestLoadAll(t *testing.T) {
	loader, err := NewLoader("../../workspace/manifests")
	if err != nil {
		t.Fatalf("NewLoader failed: %v", err)
	}

	apps, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() failed: %v", err)
	}

	minAppCount := 4 // At least nas-app, vpn-app, internal-api, nas-agent
	if len(apps) < minAppCount {
		t.Errorf("LoadAll() returned %d apps, want at least %d", len(apps), minAppCount)
	}

	for _, app := range apps {
		if app.Metadata.Name == "" {
			t.Error("LoadAll() produced app with empty name")
		}
		if app.APIVersion != "mnlab/v1" {
			t.Errorf("LoadAll() app %s has apiVersion %s, want mnlab/v1", app.Metadata.Name, app.APIVersion)
		}
		if app.Kind != "AppConfig" {
			t.Errorf("LoadAll() app %s has kind %s, want AppConfig", app.Metadata.Name, app.Kind)
		}
	}
}

// TestLoadOrdered verifies dependency-ordered loading.
func TestLoadOrdered(t *testing.T) {
	loader, err := NewLoader("../../workspace/manifests")
	if err != nil {
		t.Fatalf("NewLoader failed: %v", err)
	}

	ordered, err := loader.LoadOrdered()
	if err != nil {
		t.Fatalf("LoadOrdered() failed: %v", err)
	}

	if len(ordered) == 0 {
		t.Fatal("LoadOrdered() returned empty list")
	}

	// Verify order: dependencies come before dependents
	seen := make(map[string]int)
	for i, app := range ordered {
		seen[app.Metadata.Name] = i

		// Check that all dependencies have been seen before this app
		for _, dep := range app.Spec.Dependencies {
			if idx, found := seen[dep]; found && idx >= i {
				t.Errorf("LoadOrdered() app %s (index %d) depends on %s (index %d), wrong order",
					app.Metadata.Name, i, dep, idx)
			}
		}
	}
}

// TestValidate tests manifest validation.
func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		app     *AppConfig
		wantErr bool
	}{
		{
			name: "valid app config",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "test-app"},
				Spec: Spec{
					Runtime: Runtime{Port: 3000},
					Capabilities: Capabilities{Exposure: "internal"},
					Domains: Domains{Internal: "test.internal"},
				},
			},
			wantErr: false,
		},
		{
			name: "missing name",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{},
				Spec:       Spec{},
			},
			wantErr: true,
		},
		{
			name: "missing port",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "test"},
				Spec:       Spec{},
			},
			wantErr: true,
		},
		{
			name: "exposure internal without internal domain",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "test"},
				Spec: Spec{
					Runtime: Runtime{Port: 3000},
					Capabilities: Capabilities{Exposure: "internal"},
					Domains: Domains{Internal: ""},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.app.Validate()
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("Validate() errors = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

// TestProviderName tests OAuth2 provider name generation.
func TestProviderName(t *testing.T) {
	app := &AppConfig{
		Metadata: Metadata{Name: "my-app"},
	}
	want := "my-app-oauth2"
	if got := app.ProviderName(); got != want {
		t.Errorf("ProviderName() = %s, want %s", got, want)
	}
}

// BenchmarkLoadAll measures performance of loading all manifests.
func BenchmarkLoadAll(b *testing.B) {
	loader, _ := NewLoader("../../workspace/manifests")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loader.LoadAll()
	}
}

// BenchmarkLoadOrdered measures performance of dependency ordering.
func BenchmarkLoadOrdered(b *testing.B) {
	loader, _ := NewLoader("../../workspace/manifests")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loader.LoadOrdered()
	}
}
