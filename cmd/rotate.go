package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rotateCmd = &cobra.Command{
	Use:   "rotate-secret <app-name>",
	Short: "Rotate Authentik OAuth2 credentials and update Coolify env vars",
	Long: `Rotates the Authentik OAuth2 client credentials for an app and
updates the corresponding environment variables in Coolify without redeployment.

Steps:
  1. Generate new client ID + secret in Authentik
  2. Update Coolify service environment variables
  3. Trigger Coolify redeploy
  4. Verify app is healthy after rotation

Examples:
  mayence rotate-secret nas-app`,
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

		if app.Spec.Capabilities.Auth != "oidc" {
			return fmt.Errorf("%s does not use OIDC auth — nothing to rotate", appName)
		}

		fmt.Printf("%s rotating secrets for %s...\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName))

		// 1. Rotate in Authentik
		step("Authentik", "Regenerating client credentials")
		authentikClient := authentik.NewClient(
			viper.GetString("AUTHENTIK_ENDPOINT"),
			viper.GetString("AUTHENTIK_API_TOKEN"),
		)
		creds, err := authentikClient.RotateOAuth2Secret(ctx, appName)
		if err != nil {
			return fmt.Errorf("authentik: %w", err)
		}
		ok("Authentik", fmt.Sprintf("new credentials generated for provider %s", creds.ProviderName))

		// 2. Update Coolify env vars
		step("Coolify", "Updating environment variables")
		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_ENDPOINT"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)
		svc, err := coolifyClient.GetAppByName(ctx, appName)
		if err != nil {
			return fmt.Errorf("coolify get app: %w", err)
		}
		err = coolifyClient.UpdateEnvVars(ctx, svc.ID, map[string]string{
			appName + "_AUTHENTIK_CLIENT_ID":     creds.ClientID,
			appName + "_AUTHENTIK_CLIENT_SECRET": creds.ClientSecret,
		})
		if err != nil {
			return fmt.Errorf("coolify update env: %w", err)
		}
		ok("Coolify", "env vars updated")

		// 3. Trigger redeploy
		step("Coolify", "Triggering redeploy with new credentials")
		if err := coolifyClient.Deploy(ctx, svc.ID); err != nil {
			return fmt.Errorf("coolify deploy: %w", err)
		}

		// 4. Health check
		step("Health", "Verifying app is healthy after rotation")
		if err := coolifyClient.WaitForHealthy(ctx, svc.ID, 2*time.Minute); err != nil {
			return fmt.Errorf("health check failed after rotation: %w", err)
		}
		ok("Health", "app healthy with new credentials")

		fmt.Printf("\n%s Secret rotation complete for %s\n", color.GreenString("✓"), appName)
		return nil
	},
}
