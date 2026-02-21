package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback <app-name>",
	Short: "Roll back an app to its previous Coolify deployment",
	Long: `Rolls back the deployment of an app to the most recent previous
successful build in Coolify.

Does NOT roll back Authentik or Traefik config — only the container image.
For full manifest rollback, revert the git commit and 'mayence deploy' again.

Examples:
  mayence rollback nas-app`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		appName := args[0]

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}
		app, err := loader.LoadApp(appName)
		if err != nil {
			return err
		}

		fmt.Printf("%s rolling back %s...\n", color.YellowString("↩"), color.New(color.Bold).Sprint(appName))

		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_ENDPOINT"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)

		// Get service
		svc, err := coolifyClient.GetAppByName(ctx, appName)
		if err != nil {
			return fmt.Errorf("could not find Coolify service for %s: %w", appName, err)
		}

		// List deployments and find second-most-recent successful one
		step("Coolify", "Fetching deployment history")
		deployments, err := coolifyClient.ListDeployments(ctx, svc.ID)
		if err != nil {
			return fmt.Errorf("could not fetch deployment history: %w", err)
		}
		if len(deployments) < 2 {
			return fmt.Errorf("no previous deployment found to roll back to")
		}
		target := deployments[1] // [0] = current, [1] = previous
		fmt.Printf("  Previous deployment: %s (deployed %s)\n", target.ID, target.CreatedAt.Format("2006-01-02 15:04"))

		// Trigger rollback
		step("Coolify", fmt.Sprintf("Rolling back to deployment %s", target.ID))
		if err := coolifyClient.RollbackToDeployment(ctx, svc.ID, target.ID); err != nil {
			return fmt.Errorf("rollback failed: %w", err)
		}

		// Health check
		step("Health", "Verifying app is healthy after rollback")
		if err := coolifyClient.WaitForHealthy(ctx, svc.ID, 2*time.Minute); err != nil {
			return fmt.Errorf("health check failed after rollback: %w", err)
		}
		ok("Health", "app healthy on previous deployment")

		fmt.Printf("\n%s Rollback complete for %s\n", color.GreenString("✓"), app.Metadata.Name)
		return nil
	},
}
