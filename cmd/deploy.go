package cmd

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/github"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/traefik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/vault"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var deployStage string

// stageToProductionBranch returns true if the stage flag targets production.
func isProductionStage(stage string) bool {
	return stage == "prod" || stage == "production"
}

// applyStage overrides the manifest branch for the target stage:
//   - dev (default): use manifest branch as-is (e.g. develop)
//   - prod: override to "main" — Coolify production resource tracks the main branch
//
// Also applies environment_overrides[stage] if present, merging
// stage-specific env vars on top of the base environment.
func applyStage(app *manifest.AppConfig, stage string) {
	if isProductionStage(stage) {
		app.Spec.Repository.Branch = "main"
		app.ApplyStageOverrides("prod")
	} else {
		app.ApplyStageOverrides(stage)
	}
}

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

Use --stage prod to deploy the production environment (branch: main).
Default is dev (branch from manifest, e.g. develop).

Examples:
  mn-cli deploy hello-world
  mn-cli deploy hello-world --stage prod
  mn-cli deploy hello-world --dry-run`,
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

		// Apply stage: overrides branch to "main" for prod, no-op for dev.
		applyStage(app, deployStage)
		stageLabel := deployStage
		if isProductionStage(deployStage) {
			stageLabel = "production"
		} else {
			stageLabel = "development"
		}

		if dryRun {
			fmt.Printf("%s dry-run mode: use 'mayence plan %s' for detailed preview\n",
				color.YellowString("⚠"), appName)
			return nil
		}

		fmt.Printf("%s deploying %s [%s]...\n\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName), stageLabel)

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

		// ── 2b. Vault: persist Authentik credentials + derived URLs ─────────
		vaultClient := vault.NewClient(
			viper.GetString("BAO_ADDR"),
			viper.GetString("BAO_TOKEN"),
			viper.GetString("BAO_NAMESPACE"),
		)
		if vaultClient.Enabled() && clientID != "" {
			authentikURL := viper.GetString("AUTHENTIK_URL")
			slug := app.AppSlug()
			vaultData := map[string]string{
				"authentik_client_id":     clientID,
				"authentik_client_secret": clientSecret,
				"authentik_slug":          slug,
				"authentik_url":           authentikURL,
				"authentik_authority_url": authentikURL + "/application/o/" + slug + "/",
				"authentik_jwks_uri":      authentikURL + "/application/o/" + slug + "/jwks/",
				"authentik_issuer":        authentikURL + "/application/o/" + slug + "/",
				"authentik_logout_url":    authentikURL + "/application/o/" + slug + "/end-session/",
			}
			step("Vault", fmt.Sprintf("Saving OIDC credentials + URLs → %s", app.VaultPath()))
			err := vaultClient.KVWrite(ctx, app.VaultPath(), vaultData)
			if err != nil {
				// Non-fatal: log warning and continue deployment
				fmt.Printf("  %s [Vault] could not save credentials: %v\n",
					color.YellowString("⚠"), err)
			} else {
				ok("Vault", fmt.Sprintf("credentials saved (%d keys) to %s", len(vaultData), app.VaultPath()))
			}
		} else if !vaultClient.Enabled() && clientID != "" {
			fmt.Printf("  %s [Vault] BAO_ADDR/BAO_TOKEN not configured — skipping credential save\n",
				color.YellowString("⚠"))
		}

		// ── 2c. Vault: resolve secrets.inject → env vars ─────────────────────
		if vaultClient.Enabled() && len(app.Spec.Secrets.Inject) > 0 {
			step("Vault", fmt.Sprintf("Resolving %d secret injection(s)", len(app.Spec.Secrets.Inject)))
			injected := 0
			for _, si := range app.Spec.Secrets.Inject {
				path := si.VaultPath
				if path == "" {
					path = app.EffectiveVaultPath()
				}
				data, err := vaultClient.KVRead(ctx, path)
				if err != nil {
					fmt.Printf("  %s [Vault] read %s: %v\n", color.YellowString("⚠"), path, err)
					continue
				}
				if data == nil {
					fmt.Printf("  %s [Vault] %s not found — skipping %s\n",
						color.YellowString("⚠"), path, si.Env)
					continue
				}
				val, exists := data[si.VaultKey]
				if !exists {
					fmt.Printf("  %s [Vault] key %q not found in %s — skipping %s\n",
						color.YellowString("⚠"), si.VaultKey, path, si.Env)
					continue
				}
				if app.Spec.Environment == nil {
					app.Spec.Environment = make(manifest.Env)
				}
				app.Spec.Environment[si.Env] = fmt.Sprintf("%v", val)
				injected++
			}
			ok("Vault", fmt.Sprintf("%d/%d secrets injected into env", injected, len(app.Spec.Secrets.Inject)))
		}

		// ── 2d. Vault: resolve ${vault:path#key} refs in environment ─────────
		if vaultClient.Enabled() {
			vaultRefCache := make(map[string]map[string]interface{})
			for envKey, envVal := range app.Spec.Environment {
				resolved, err := resolveVaultRefs(ctx, vaultClient, envVal, vaultRefCache)
				if err != nil {
					fmt.Printf("  %s [Vault] resolving %s: %v\n", color.YellowString("⚠"), envKey, err)
					continue
				}
				if resolved != envVal {
					app.Spec.Environment[envKey] = resolved
				}
			}
		}

		// ── 3. Coolify service ────────────────────────────────────────────────
		step("Coolify", "Creating/updating service")
		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_URL"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)
		// Inject Authentik credentials into env if present.
		// Replace hyphens with underscores — shell variable names cannot contain hyphens.
		if clientID != "" {
			envPrefix := strings.ReplaceAll(strings.ToUpper(app.Metadata.Name), "-", "_")
			app.Spec.Environment[envPrefix+"_AUTHENTIK_CLIENT_ID"] = clientID
			app.Spec.Environment[envPrefix+"_AUTHENTIK_CLIENT_SECRET"] = clientSecret
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

		// ── 3b. Sync environment variables to Coolify ─────────────────────────
		if len(app.Spec.Environment) > 0 {
			step("Coolify", fmt.Sprintf("Syncing %d environment variable(s)", len(app.Spec.Environment)))
			if err := coolifyClient.UpdateEnvVars(ctx, appID, app.Spec.Environment); err != nil {
				return fmt.Errorf("coolify env vars: %w", err)
			}
			ok("Coolify", fmt.Sprintf("%d env var(s) synced", len(app.Spec.Environment)))
		}

		// ── 3c. Traefik public routers (managed file) ────────────────────────
		if app.Spec.Type == "coolify-app" && app.NormalizedDomains().Public != "" {
			step("Traefik", "Reconciling managed public app routers")
			traefikClient := traefik.NewClient(resolveTraefikConfigDir(manifestsDir))
			if err := traefikClient.SyncManagedPublicRouters(app); err != nil {
				return fmt.Errorf("traefik managed routers: %w", err)
			}
			ok("Traefik", "managed public routers synced")
		}

		// ── 4. Trigger Coolify deploy ─────────────────────────────────────────
		step("Coolify", "Triggering deployment")
		if err := coolifyClient.Deploy(ctx, appID); err != nil {
			return fmt.Errorf("coolify deploy: %w", err)
		}

		// ── 5. Wait for healthy ────────────────────────────────────────────────
		domainHint := app.GetDomains().Private
		if domainHint == "" {
			domainHint = app.GetDomains().Public
		}
		step("Health", fmt.Sprintf("Waiting for %s to become healthy", domainHint))
		if err := coolifyClient.WaitForHealthy(ctx, appID, 5*time.Minute); err != nil {
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
				// (typically 2: development + production)
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
		if domains.Private != "" {
			fmt.Printf("  Private: https://%s\n", domains.Private)
		}
		if domains.Public != "" {
			fmt.Printf("  Public: https://%s\n", domains.Public)
		}

		// For non-Coolify apps (e.g. systemd-service, internet-agent, vpn-agent)
		// the wildcard *.apps.mayencenouvelle.internal does NOT cover them.
		// Print a manual step note if DNS is required.
		if app.Spec.Capabilities.DNS && app.Spec.Type != "coolify-app" {
			fmt.Printf("\n%s DNS rewrite required (AdGuard Home → Filters → DNS rewrites):\n",
				color.YellowString("⚠"))
			if domains.Private != "" {
				fmt.Printf("  %s → <node-ip>\n", color.CyanString(domains.Private))
			}
			if domains.Public != "" {
				fmt.Printf("  %s → <node-ip>\n", color.CyanString(domains.Public))
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

func init() {
	deployCmd.Flags().StringVar(&deployStage, "stage", "dev", "Deployment stage: dev or prod (default: dev)")
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

// vaultRefPattern matches ${vault:mn/data/path#key} references in env values.
var vaultRefPattern = regexp.MustCompile(`\$\{vault:([^#}]+)#([^}]+)\}`)

// resolveVaultRefs replaces all ${vault:path#key} references in a string
// with actual values from OpenBao. Uses a cache to avoid repeated reads
// of the same vault path.
func resolveVaultRefs(
	ctx context.Context,
	vc *vault.Client,
	value string,
	cache map[string]map[string]interface{},
) (string, error) {
	if !strings.Contains(value, "${vault:") {
		return value, nil
	}

	var lastErr error
	resolved := vaultRefPattern.ReplaceAllStringFunc(value, func(match string) string {
		subs := vaultRefPattern.FindStringSubmatch(match)
		if len(subs) != 3 {
			return match
		}
		path, key := subs[1], subs[2]

		// Check cache first
		data, cached := cache[path]
		if !cached {
			var err error
			data, err = vc.KVRead(ctx, path)
			if err != nil {
				lastErr = fmt.Errorf("read %s: %w", path, err)
				return match
			}
			if data == nil {
				lastErr = fmt.Errorf("secret %s not found", path)
				return match
			}
			cache[path] = data
		}

		val, ok := data[key]
		if !ok {
			lastErr = fmt.Errorf("key %q not found in %s", key, path)
			return match
		}
		return fmt.Sprintf("%v", val)
	})

	return resolved, lastErr
}
