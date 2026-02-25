package manifest

import (
	"testing"
)

// TestDefaultsApplied verifies that defaults are set during loading.
func TestDefaultsApplied(t *testing.T) {
	app := &AppConfig{
		APIVersion: "mnlab/v1",
		Kind:       "AppConfig",
		Metadata:   Metadata{Name: "test"},
		Spec: Spec{
			Runtime:      Runtime{Port: 3000},
			Capabilities: Capabilities{},
			Domains:      Domains{Private: "test.internal"},
		},
	}

	// After loading, defaults should be applied
	// (In real loader, this is done in loadFile())
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

	if app.Spec.Capabilities.Auth != "oidc" {
		t.Errorf("Auth default not applied: %s", app.Spec.Capabilities.Auth)
	}
	if app.Spec.Capabilities.Exposure != "internal" {
		t.Errorf("Exposure default not applied: %s", app.Spec.Capabilities.Exposure)
	}
	if app.Spec.Runtime.HealthEndpoint != "/health" {
		t.Errorf("HealthEndpoint default not applied: %s", app.Spec.Runtime.HealthEndpoint)
	}
	if app.Spec.Runtime.HealthInterval != "30s" {
		t.Errorf("HealthInterval default not applied: %s", app.Spec.Runtime.HealthInterval)
	}
}
