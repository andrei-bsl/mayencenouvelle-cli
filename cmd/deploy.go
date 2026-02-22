package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/github"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var deployCmd = &cobra.Command{
	Use:   "deploy <app-name>",
	Short: "Deploy a single app end-to-end (Coolify + Authentik + DNS + webhook)",
	Long: `Reads the app manifest and applies required configuration:

  1. Validates the manifest
  2. Creates/updates Authentik OAuth2 provider + application (if auth: oidc)
  3. Creates/updates Coolify service with env vars (including OIDC credentials)
  4. Triggers initial deployment in Coolify
  5. Waits for health check to pass
  6. Registers GitHub webhooks (if webhooks: true) — one per Coolify resource
     (dev + prod when both environments are deployed)

For non-Coolify apps (systemd-service, etc.) a note is printed if DNS rewrites are needed.
Wildcard *.apps.mayencenouvelle.internal covers Coolify apps — only custom hostnames
need a manual AdGuard DNS rewrite.

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
		authentikClient := authentik.NewClient(
			viper.GetString("AUTHENTIK_URL"),
			viper.GetString("AUTHENTIK_API_TOKEN"),
		)
		if app.Spec.Capabilities.Auth == "oidc" {
			step("Authentik", "Reconciling OAuth2 provider + application")
			creds, err := authentikClient.EnsureOAuth2Provider(ctx, app, base)
			if err != nil {
				return fmt.Errorf("authentik: %w", err)
			}
			clientID = creds.ClientID
			clientSecret = creds.ClientSecret
			action := "updated"
			if creds.Created {
				action = "created"
			}
			ok("Authentik", fmt.Sprintf("provider %s %s", creds.ProviderName, action))
		} else {
			// If auth was previously oidc and has been removed from the manifest,
			// clean up any stale Authentik resources (idempotent — no-op if absent).
			if err := authentikClient.DeleteOIDC(ctx, appName); err != nil {
				// Non-fatal: log and continue (resource may not exist)
				fmt.Printf("  %s [Authentik] cleanup skipped: %v\n", color.YellowString("⚠"), err)
			}
		}

		// ── 3. Coolify service ────────────────────────────────────────────────
		step("Coolify", "Creating/updating service")
		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_URL"),
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

		// ── 6. GitHub webhooks ──────────────────────────────────────────────────
		// Register one webhook per Coolify resource for this app.
		// Multiple resources exist when both dev + prod environments are deployed.
		// Each resource has its own Coolify token (ManualWebhookSecretGithub) which
		// serves as both the URL token and the HMAC signing secret for GitHub.
		if app.Spec.Capabilities.Webhooks {
			githubToken := viper.GetString("GITHUB_TOKEN")
			if githubToken == "" {
				fmt.Printf("  %s [Webhooks] GITHUB_TOKEN not set — skipping webhook registration\n",
					color.YellowString("⚠"))
			} else {
				step("Webhooks", "Reconciling GitHub webhooks")
				ghClient := github.NewClient(githubToken)
				repo := github.RepoSlug(app.Spec.Repository.URL)

				// GetAppsByName returns ALL Coolify resources with this name
				// (typically 2: development-internal + production-internal)
				allResources, err := coolifyClient.GetAppsByName(ctx, appName)
				if err != nil {
					fmt.Printf("  %s [Webhooks] could not list Coolify resources: %v\n",
						color.YellowString("⚠"), err)
				} else {
					for i := range allResources {
						resource := &allResources[i]
						// Ensure the resource has a webhook token — generate + PATCH one if missing.
						if _, err := coolifyClient.EnsureWebhookToken(ctx, resource); err != nil {
							fmt.Printf("  %s [Webhooks] could not provision token for %s: %v\n",
								color.YellowString("⚠"), resource.UUID, err)
							continue
						}
						wURL := coolifyClient.WebhookURL(resource.ManualWebhookSecretGithub)
						_, created, err := ghClient.EnsureWebhook(ctx, repo, wURL, resource.ManualWebhookSecretGithub)
						if err != nil {
							fmt.Printf("  %s [Webhooks] branch=%s: %v\n",
								color.YellowString("⚠"), resource.Branch, err)
							continue
						}
						action := "updated"
						if created {
							action = "created"
						}
						ok("Webhooks", fmt.Sprintf("%s (branch: %s, uuid: %s)",
							action, resource.Branch, resource.UUID))
					}
				}
			}
		}

		// ── Done ────────────────────────────────────────────────────────────────
		domains := app.GetDomains()

		fmt.Printf("\n%s %s deployed successfully!\n", color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
		if domains.Internal != "" {
			fmt.Printf("  Internal: https://%s\n", domains.Internal)
		}
		if domains.External != "" {
			fmt.Printf("  External: https://%s\n", domains.External)
		}

		// For non-Coolify apps (e.g. systemd-service, internet-agent, vpn-agent)
		// the wildcard *.apps.mayencenouvelle.internal does NOT cover them.
		// Print a manual step note if DNS is required.
		if app.Spec.Capabilities.DNS && app.Spec.Type != "coolify-app" {
			fmt.Printf("\n%s DNS rewrite required (AdGuard Home → Filters → DNS rewrites):\n",
				color.YellowString("⚠"))
			if domains.Internal != "" {
				fmt.Printf("  %s → <node-ip>\n", color.CyanString(domains.Internal))
			}
			if domains.External != "" {
				fmt.Printf("  %s → <node-ip>\n", color.CyanString(domains.External))
			}
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
