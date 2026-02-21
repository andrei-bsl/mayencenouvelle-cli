package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/traefik"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var planCmd = &cobra.Command{
	Use:   "plan [app-name]",
	Short: "Preview what 'deploy' would change without applying",
	Long: `Reads the app manifest and shows a diff-like preview of what
'mayence deploy' would create, update, or delete in:
  - Coolify (service configuration)
  - Authentik (OAuth2 provider + application)
  - Traefik (generated router + middleware config)
  - DNS (AdGuard Home rewrite rules)
  - GitHub (webhook configuration)

No changes are made. Safe to run at any time.

Examples:
  mayence plan nas-app
  mayence plan                  Preview all apps (same as apply-manifest --dry-run)`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

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

		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_ENDPOINT"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)
		authentikClient := authentik.NewClient(
			viper.GetString("AUTHENTIK_ENDPOINT"),
			viper.GetString("AUTHENTIK_API_TOKEN"),
		)
		traefikClient := traefik.NewClient(
			viper.GetString("TRAEFIK_CONFIG_DIR"),
		)

		for _, app := range apps {
			if !app.Spec.Enabled {
				fmt.Printf("  ⏸  %s (disabled — skipping)\n", app.Metadata.Name)
				continue
			}

			fmt.Printf("\n%s\n", color.CyanString("═══ %s ═══", app.Metadata.Name))

			// Coolify diff
			printSection("Coolify")
			coolifyPlan, err := coolifyClient.PlanApp(ctx, app)
			if err != nil {
				printError("Coolify", err)
			} else {
				printPlanActions(coolifyPlan)
			}

			// Authentik diff (only oidc apps)
			if app.Spec.Capabilities.Auth == "oidc" {
				printSection("Authentik")
				authentikPlan, err := authentikClient.PlanOAuth2Provider(ctx, app)
				if err != nil {
					printError("Authentik", err)
				} else {
					printPlanActions(authentikPlan)
				}
			}

			// Traefik diff
			printSection("Traefik")
			traefikPlan, err := traefikClient.PlanRoutes(app)
			if err != nil {
				printError("Traefik", err)
			} else {
				printPlanActions(traefikPlan)
			}
		}

		fmt.Printf("\n%s\n", color.YellowString("Dry-run complete. Use 'mayence deploy' to apply."))
		return nil
	},
}

// PlanAction represents a single planned operation.
type PlanAction struct {
	Operation string // create | update | delete | no-change
	Resource  string
	Detail    string
}

func printSection(name string) {
	fmt.Printf("  %s\n", color.BlueString("[%s]", name))
}

func printError(section string, err error) {
	fmt.Printf("    %s %s: %v\n", color.RedString("✗"), section, err)
}

func printPlanActions(actions []PlanAction) {
	for _, a := range actions {
		switch a.Operation {
		case "create":
			fmt.Printf("    %s %s: %s\n", color.GreenString("+"), a.Resource, a.Detail)
		case "update":
			fmt.Printf("    %s %s: %s\n", color.YellowString("~"), a.Resource, a.Detail)
		case "delete":
			fmt.Printf("    %s %s: %s\n", color.RedString("-"), a.Resource, a.Detail)
		default:
			fmt.Printf("    %s %s\n", color.WhiteString("="), a.Resource)
		}
	}
}
