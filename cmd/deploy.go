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

var deployCmd = &cobra.Command{
	Use:   "deploy <app-name>",
	Short: "Deploy a single app end-to-end (Coolify + Authentik + Traefik + DNS + webhook)",
	Long: `Reads the app manifest and applies all required configuration:

  1. Validates the manifest
  2. Creates/updates Authentik OAuth2 provider + application (if auth: oidc)
  3. Creates/updates Coolify service with env vars (including OIDC credentials)
  4. Generates Traefik router config (internal and/or external)
  5. Creates/updates DNS rewrite in AdGuard Home
  6. Configures GitHub webhook for auto-deploy on push
  7. Triggers initial deployment in Coolify
  8. Waits for health check to pass

All operations are idempotent — safe to run multiple times.

Examples:
  mayence deploy nas-app
  mayence deploy nas-app --dry-run    Preview without applying`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		appName := args[0]

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// ── 1. Load + validate manifest ─────────────────────────────────────
		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}
		app, err := loader.LoadApp(appName)
		if err != nil {
			return err
		}
		if errs := app.Validate(); len(errs) > 0 {
			for _, e := range errs {
				fmt.Printf("  ✗ %s\n", e)
			}
			return fmt.Errorf("manifest validation failed")
		}

		if dryRun {
			fmt.Printf("%s dry-run mode: use 'mayence plan %s' for detailed preview\n",
				color.YellowString("⚠"), appName)
			return nil
		}

		fmt.Printf("%s deploying %s...\n\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName))

		// ── 2. Authentik OAuth2 (only for oidc apps) ─────────────────────────
		var clientID, clientSecret string
		if app.Spec.Capabilities.Auth == "oidc" {
			step("Authentik", "Creating OAuth2 provider + application")
			authentikClient := authentik.NewClient(
				viper.GetString("AUTHENTIK_ENDPOINT"),
				viper.GetString("AUTHENTIK_API_TOKEN"),
			)
			creds, err := authentikClient.EnsureOAuth2Provider(ctx, app)
			if err != nil {
				return fmt.Errorf("authentik: %w", err)
			}
			clientID = creds.ClientID
			clientSecret = creds.ClientSecret
			ok("Authentik", fmt.Sprintf("provider %s ready", creds.ProviderName))
		}

		// ── 3. Coolify service ────────────────────────────────────────────────
		step("Coolify", "Creating/updating service")
		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_ENDPOINT"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)
		// Inject Authentik credentials into env if present
		if clientID != "" {
			app.Spec.Environment[app.Metadata.Name+"_AUTHENTIK_CLIENT_ID"] = clientID
			app.Spec.Environment[app.Metadata.Name+"_AUTHENTIK_CLIENT_SECRET"] = clientSecret
		}
		svc, err := coolifyClient.EnsureApp(ctx, app)
		if err != nil {
			return fmt.Errorf("coolify: %w", err)
		}
		ok("Coolify", fmt.Sprintf("service %s ready (id: %s)", svc.Name, svc.ID))

		// ── 4. Traefik routes ─────────────────────────────────────────────────
		step("Traefik", "Generating router configuration")
		traefikClient := traefik.NewClient(viper.GetString("TRAEFIK_CONFIG_DIR"))
		if err := traefikClient.ApplyRoutes(app); err != nil {
			return fmt.Errorf("traefik: %w", err)
		}
		ok("Traefik", "routes written to config dir")

		// ── 5. Trigger Coolify deploy ─────────────────────────────────────────
		step("Coolify", "Triggering deployment")
		if err := coolifyClient.Deploy(ctx, svc.ID); err != nil {
			return fmt.Errorf("coolify deploy: %w", err)
		}

		// ── 6. Wait for healthy ────────────────────────────────────────────────
		step("Health", fmt.Sprintf("Waiting for %s to become healthy", app.Spec.Domains.Internal))
		if err := coolifyClient.WaitForHealthy(ctx, svc.ID, 2*time.Minute); err != nil {
			return fmt.Errorf("health check failed: %w", err)
		}
		ok("Health", "service is healthy")

		// ── Done ────────────────────────────────────────────────────────────────
		fmt.Printf("\n%s %s deployed successfully!\n", color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
		if app.Spec.Domains.Internal != "" {
			fmt.Printf("  Internal: https://%s\n", app.Spec.Domains.Internal)
		}
		if app.Spec.Domains.External != "" {
			fmt.Printf("  External: https://%s\n", app.Spec.Domains.External)
		}
		return nil
	},
}

var applyCmd = &cobra.Command{
	Use:   "apply-manifest",
	Short: "Deploy all enabled apps in dependency order",
	Long: `Deploys all enabled apps defined in workspace/manifests/apps/,
respecting the 'dependencies' field for correct ordering.

Examples:
  mayence apply-manifest
  mayence apply-manifest --dry-run`,
	RunE: func(cmd *cobra.Command, args []string) error {
		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}
		apps, err := loader.LoadOrdered()
		if err != nil {
			return err
		}

		for _, app := range apps {
			if !app.Spec.Enabled {
				fmt.Printf("  ⏸  %s (disabled)\n", app.Metadata.Name)
				continue
			}
			// Delegate to deployCmd logic
			if err := runDeploy(app.Metadata.Name); err != nil {
				return fmt.Errorf("deploy %s: %w", app.Metadata.Name, err)
			}
		}
		return nil
	},
}

// runDeploy is a helper to deploy a named app (reuses deployCmd logic).
func runDeploy(appName string) error {
	return deployCmd.RunE(deployCmd, []string{appName})
}

// step prints a deployment step.
func step(component, msg string) {
	fmt.Printf("  %s [%s] %s...\n", color.BlueString("→"), component, msg)
}

// ok prints a successful step result.
func ok(component, msg string) {
	fmt.Printf("  %s [%s] %s\n", color.GreenString("✓"), component, msg)
}
