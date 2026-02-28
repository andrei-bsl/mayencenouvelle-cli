package manifest

import (
	"testing"
)

// TestAppConfigValidate tests the app config validation logic.
func TestAppConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		app     *AppConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "minimal valid config",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "app1"},
				Spec: Spec{
					Type:         "coolify-app",
					Runtime:      Runtime{Port: 3000},
					Capabilities: Capabilities{Exposure: "internal"},
					Domains:      Domains{Private: "app1.internal"},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid apiVersion",
			app: &AppConfig{
				APIVersion: "v2",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "app1"},
			},
			wantErr: true,
			errMsg:  "apiVersion must be 'mnlab/v1'",
		},
		{
			name: "missing private and public domains",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "app1"},
				Spec: Spec{
					Type:         "coolify-app",
					Runtime:      Runtime{Port: 3000},
					Capabilities: Capabilities{Exposure: "both"},
					Domains:      Domains{},
				},
			},
			wantErr: true,
			errMsg:  "at least one of domains.private or domains.public is required",
		},
		{
			name: "systemd-service without node",
			app: &AppConfig{
				APIVersion: "mnlab/v1",
				Kind:       "AppConfig",
				Metadata:   Metadata{Name: "app1"},
				Spec: Spec{
					Type:         "systemd-service",
					Runtime:      Runtime{Port: 3000},
					Capabilities: Capabilities{Exposure: "internal"},
					Domains:      Domains{Private: "app.internal"},
				},
			},
			wantErr: true,
			errMsg:  "spec.node is required for systemd-service type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.app.Validate()
			if (len(errs) > 0) != tt.wantErr {
				t.Fatalf("Validate() errors = %v, wantErr %v", errs, tt.wantErr)
			}
			if tt.wantErr && len(errs) > 0 {
				found := false
				for _, err := range errs {
					if err == tt.errMsg {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Validate() expected error %q, got %v", tt.errMsg, errs)
				}
			}
		})
	}
}

// TestProviderNameGeneration verifies OAuth2 provider naming.
func TestProviderNameGeneration(t *testing.T) {
	tests := []struct {
		appName string
		want    string
	}{
		{"nas-app", "mn-nas-app-provider"},
		{"vpn-app", "mn-vpn-app-provider"},
		{"internal-api", "mn-internal-api-provider"},
	}

	for _, tt := range tests {
		t.Run(tt.appName, func(t *testing.T) {
			app := &AppConfig{Metadata: Metadata{Name: tt.appName}}
			if got := app.ProviderName(); got != tt.want {
				t.Errorf("ProviderName() = %q, want %q", got, tt.want)
			}
		})
	}
}
