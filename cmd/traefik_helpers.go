package cmd

import (
	"path/filepath"

	"github.com/spf13/viper"
)

// resolveTraefikConfigDir returns the dynamic config directory used by Traefik.
// Priority:
//  1. explicit env/config TRAEFIK_CONFIG_DIR
//  2. lab-repo default relative to manifests: ../../configs/stage-6-platform-stack/traefik/dynamic
func resolveTraefikConfigDir(manifestsBase string) string {
	if configured := viper.GetString("TRAEFIK_CONFIG_DIR"); configured != "" {
		return configured
	}
	if manifestsBase == "" {
		return ""
	}
	return filepath.Join(manifestsBase, "..", "..", "configs", "stage-6-platform-stack", "traefik", "dynamic")
}
