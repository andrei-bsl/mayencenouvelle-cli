package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var deployCmd = &cobra.Command{
	Use:   "deploy <app-name>",
	Short: "Deploy a single app end-to-end (Coolify + Authentik + DNS + webhook)",
	Long: `Reads the app manifest and applies all required configuration:

  1. Validates the manifest
  2. Creates/updates Authentik OAuth2 provider + application (if auth: oidc)
  3. Creates/updates Coolify service with env vars (including OIDC credentials)
  4. Triggers initial deployment in Coolify
  5. Waits for health check to pass

  Note: domain routing (FQDN) must be set manually in Coolify UI after first deploy.
  Coolify API does not allow setting fqdn via API — routing is handled by Coolify's
  internal Traefik proxy using the wildcard *.apps.mayencenouvelle.internal rule.

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
		base, err := loader.LoadBase()
		if err != nil {
			return fmt.Errorf("loading base config: %w", err)
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
		svc, err := coolifyClient.EnsureApp(ctx, app, base)
		if err != nil {
			return fmt.Errorf("coolify: %w", err)
		}
		// Use UUID for API operations (Coolify returns uuid, not id)
		appID := svc.UUID
		if appID == "" {
			appID = svc.ID
		}
		ok("Coolify", fmt.Sprintf("service %s ready (uuid: %s)", svc.Name, appID))

		// ── 4. Trigger Coolify deploy ─────────────────────────────────────────
		step("Coolify", "Triggering deployment")
		if err := coolifyClient.Deploy(ctx, appID); err != nil {
			return fmt.Errorf("coolify deploy: %w", err)
		}

		// ── 5. Wait for healthy ────────────────────────────────────────────────
		step("Health", fmt.Sprintf("Waiting for %s to become healthy", app.Spec.Domains.Internal))
		if err := coolifyClient.WaitForHealthy(ctx, appID, 2*time.Minute); err != nil {
			return fmt.Errorf("health check failed: %w", err)
		}
		ok("Health", "service is healthy")

		// ── Done ────────────────────────────────────────────────────────────────
		domains := app.GetDomains()
		coolifyURL := viper.GetString("COOLIFY_ENDPOINT")

		fmt.Printf("\n%s %s deployed successfully!\n", color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
		if domains.Internal != "" {
			fmt.Printf("  Internal: https://%s\n", domains.Internal)
		}
		if domains.External != "" {
			fmt.Printf("  External: https://%s\n", domains.External)
		}

		// Warn if the FQDN in Coolify doesn't include the expected domain (first deploy only).
		// Coolify may store the FQDN with http:// even when https:// is used — normalise
		// by stripping the scheme and checking only the host portion.
		stripScheme := func(u string) string {
			u = strings.TrimPrefix(u, "https://")
			u = strings.TrimPrefix(u, "http://")
			return u
		}
		fqdnHosts := stripScheme(svc.FQDN) // may be comma-separated for multi-domain apps
		expectedHost := domains.Internal
		fqdnSet := expectedHost != "" && strings.Contains(fqdnHosts, expectedHost)
		if !fqdnSet {
			fmt.Printf("\n%s Manual step required (first deploy only):\n", color.YellowString("⚠"))
			fmt.Printf("  Set the domain in Coolify UI → %s → Settings → Domains:\n", appName)
			fmt.Printf("  %s\n", color.CyanString("https://"+domains.Internal))
			fmt.Printf("  Coolify UI: %s\n", coolifyURL)
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
