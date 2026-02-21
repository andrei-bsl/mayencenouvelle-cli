package cmd

import (
	"fmt"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate [app-name]",
	Short: "Validate app manifests against the mnlab/v1 schema",
	Long: `Validates one or all app manifests against the JSONSchema.
Fails with non-zero exit code if any manifest is invalid — used as CI gate.

Examples:
  mayence validate             Validate all manifests
  mayence validate nas-app     Validate a single app`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}

		var apps []*manifest.AppConfig
		if len(args) == 1 {
			app, err := loader.LoadApp(args[0])
			if err != nil {
				return err
			}
			apps = []*manifest.AppConfig{app}
		} else {
			apps, err = loader.LoadAll()
			if err != nil {
				return err
			}
		}

		valid := true
		for _, app := range apps {
			errs := app.Validate()
			if len(errs) == 0 {
				color.Green("  ✓ %s", app.Metadata.Name)
			} else {
				color.Red("  ✗ %s", app.Metadata.Name)
				for _, e := range errs {
					fmt.Printf("    → %s\n", e)
				}
				valid = false
			}
		}

		if !valid {
			return fmt.Errorf("one or more manifests failed validation")
		}

		color.Green("\n✓ All manifests valid")
		return nil
	},
}
