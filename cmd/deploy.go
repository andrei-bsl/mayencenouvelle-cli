package cmd

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/authentik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/database"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/github"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/traefik"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/vault"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var deployStage string
var redeployStage string

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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return runDeployWithDependents(ctx, args[0], deployStage, true, map[string]bool{})
	},
}

var redeployCmd = &cobra.Command{
	Use:   "redeploy <app-name>",
	Short: "Redeploy a single app and refresh dependent consumers",
	Long: `Re-applies manifest configuration and triggers a fresh Coolify deployment.

This is equivalent to a full manifest-driven deploy without manually stopping
the app first. When the app exports OIDC or other vault-backed values consumed
by other apps, same-stage dependents are redeployed automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return runDeployWithDependents(ctx, args[0], redeployStage, true, map[string]bool{})
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
	redeployCmd.Flags().StringVar(&redeployStage, "stage", "dev", "Deployment stage: dev or prod (default: dev)")
	rootCmd.AddCommand(redeployCmd)
}

// runDeploy is a helper to deploy a named app (reuses deployCmd logic).
func runDeploy(appName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return runDeployWithDependents(ctx, appName, deployStage, false, map[string]bool{})
}

func runDeployWithDependents(ctx context.Context, appName, stage string, cascadeDependents bool, visited map[string]bool) error {
	key := stage + ":" + appName
	if visited[key] {
		return nil
	}
	visited[key] = true

	deployedApp, loader, err := deploySingleApp(ctx, appName, stage)
	if err != nil {
		return err
	}
	if !cascadeDependents {
		return nil
	}

	dependents, err := findVaultDependentApps(loader, deployedApp)
	if err != nil {
		return fmt.Errorf("finding dependent apps for %s: %w", appName, err)
	}
	if len(dependents) == 0 {
		return nil
	}

	sort.Strings(dependents)
	coolifyClient := coolify.NewClient(
		viper.GetString("COOLIFY_URL"),
		viper.GetString("COOLIFY_API_TOKEN"),
	)

	for _, dependentName := range dependents {
		dependentApp, err := loader.LoadApp(dependentName)
		if err != nil {
			return fmt.Errorf("load dependent app %s: %w", dependentName, err)
		}
		applyStage(dependentApp, stage)
		svc, err := coolifyClient.GetAppByNameAndBranch(ctx, dependentName, dependentApp.Spec.Repository.Branch)
		if err != nil {
			return fmt.Errorf("check dependent app %s: %w", dependentName, err)
		}
		if svc == nil {
			fmt.Printf("  %s [Dependents] %s references %s vault data, but no %s-stage Coolify resource exists — skipping auto-redeploy\n",
				color.YellowString("⚠"), dependentName, appName, stage)
			continue
		}
		step("Dependents", fmt.Sprintf("Redeploying %s because it consumes %s vault data", dependentName, appName))
		if err := runDeployWithDependents(ctx, dependentName, stage, false, visited); err != nil {
			return fmt.Errorf("redeploy dependent %s: %w", dependentName, err)
		}
		ok("Dependents", fmt.Sprintf("%s refreshed", dependentName))
	}

	return nil
}

func deploySingleApp(ctx context.Context, appName, stage string) (*manifest.AppConfig, *manifest.Loader, error) {
	loader, err := manifest.NewLoader(manifestsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("loading manifests: %w", err)
	}
	app, err := loader.LoadApp(appName)
	if err != nil {
		return nil, nil, err
	}
	base, err := loader.LoadBase()
	if err != nil {
		return nil, nil, fmt.Errorf("loading base config: %w", err)
	}
	if errs := app.Validate(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Printf("  ✗ %s\n", e)
		}
		return nil, nil, fmt.Errorf("manifest validation failed")
	}

	applyStage(app, stage)
	stageLabel := "development"
	if isProductionStage(stage) {
		stageLabel = "production"
	}

	if dryRun {
		fmt.Printf("%s dry-run mode: use 'mayence plan %s' for detailed preview\n",
			color.YellowString("⚠"), appName)
		return app, loader, nil
	}

	fmt.Printf("%s deploying %s [%s]...\n\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName), stageLabel)

	var clientID, clientSecret string
	authentikClient := authentik.NewClient(
		viper.GetString("AUTHENTIK_URL"),
		viper.GetString("AUTHENTIK_API_TOKEN"),
	)
	if app.Spec.Capabilities.Auth == "oidc" {
		step("Authentik", "Reconciling OAuth2 provider + application")
		creds, err := authentikClient.EnsureOAuth2Provider(ctx, app, base)
		if err != nil {
			return nil, nil, fmt.Errorf("authentik: %w", err)
		}
		clientID = creds.ClientID
		clientSecret = creds.ClientSecret
		action := "updated"
		if creds.Created {
			action = "created"
		}
		ok("Authentik", fmt.Sprintf("provider %s %s", creds.ProviderName, action))
	} else {
		if err := authentikClient.DeleteOIDC(ctx, appName); err != nil {
			fmt.Printf("  %s [Authentik] cleanup skipped: %v\n", color.YellowString("⚠"), err)
		}
	}

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
			fmt.Printf("  %s [Vault] could not save credentials: %v\n", color.YellowString("⚠"), err)
		} else {
			ok("Vault", fmt.Sprintf("credentials saved (%d keys) to %s", len(vaultData), app.VaultPath()))
		}
	} else if !vaultClient.Enabled() && clientID != "" {
		fmt.Printf("  %s [Vault] BAO_ADDR/BAO_TOKEN not configured — skipping credential save\n",
			color.YellowString("⚠"))
	}

	// ── Database Bootstrap ────────────────────────────────────────────────────
	// Runs after Authentik (so vault path is established) and before the
	// secrets inject pass so DATABASE_URL is set in app.Spec.Environment.
	if app.Spec.Database.Enabled {
		if err := runDatabaseBootstrap(ctx, app, base, loader, vaultClient); err != nil {
			return nil, nil, fmt.Errorf("database bootstrap: %w", err)
		}
	}

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

	step("Coolify", "Creating/updating service")
	coolifyClient := coolify.NewClient(
		viper.GetString("COOLIFY_URL"),
		viper.GetString("COOLIFY_API_TOKEN"),
	)
	svc, err := coolifyClient.EnsureApp(ctx, app, base)
	if err != nil {
		return nil, nil, fmt.Errorf("coolify: %w", err)
	}
	appID := svc.UUID
	if appID == "" {
		appID = svc.ID
	}
	ok("Coolify", fmt.Sprintf("service %s ready (uuid: %s)", svc.Name, appID))

	if len(app.Spec.Environment) > 0 {
		step("Coolify", fmt.Sprintf("Syncing %d environment variable(s)", len(app.Spec.Environment)))
		if err := coolifyClient.UpdateEnvVars(ctx, appID, app.Spec.Environment); err != nil {
			return nil, nil, fmt.Errorf("coolify env vars: %w", err)
		}
		ok("Coolify", fmt.Sprintf("%d env var(s) synced", len(app.Spec.Environment)))
	}

	if app.Spec.Type == "coolify-app" && app.NormalizedDomains().Public != "" {
		traefikDir := resolveTraefikConfigDir(manifestsDir)
		step("Traefik", "Enforcing single source-of-truth for public routers")
		if err := enforceSinglePublicRouterSource(traefikDir); err != nil {
			return nil, nil, fmt.Errorf("traefik public router ownership: %w", err)
		}
		ok("Traefik", "single source-of-truth check passed")

		traefikClient := traefik.NewClient(traefikDir)
		if wildcardModeEnabled() {
			step("Traefik", "Wildcard public mode enabled (skipping per-app router generation)")
			if err := traefikClient.RebuildManagedPublicRouters(nil); err != nil {
				return nil, nil, fmt.Errorf("traefik managed routers cleanup: %w", err)
			}
			apiURL := strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_URL"))
			if apiURL == "" {
				apiURL = strings.TrimSpace(base.Traefik.AdminEndpoint)
			}
			insecure := strings.EqualFold(strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_INSECURE")), "true")
			if err := verifyWildcardPublicRouter(ctx, apiURL, insecure); err != nil {
				return nil, nil, fmt.Errorf("traefik wildcard router verification: %w", err)
			}
			ok("Traefik", "wildcard public router verified")
		} else {
			step("Traefik", "Rebuilding managed public app routers from manifests")
			allApps, err := loader.LoadOrdered()
			if err != nil {
				return nil, nil, fmt.Errorf("loading manifests for traefik rebuild: %w", err)
			}
			if err := traefikClient.RebuildManagedPublicRouters(allApps); err != nil {
				return nil, nil, fmt.Errorf("traefik managed routers rebuild: %w", err)
			}
			ok("Traefik", "managed public routers rebuilt")
		}

		step("Traefik", "Syncing router files to runtime host")
		synced, err := syncTraefikRuntimeFiles(traefikDir)
		if err != nil {
			return nil, nil, fmt.Errorf("traefik runtime sync: %w", err)
		}
		if synced {
			ok("Traefik", "runtime dynamic config synced")
		} else {
			fmt.Printf("  %s [Traefik] runtime sync skipped (set MN_TRAEFIK_RUNTIME_SSH_TARGET or MN_RUNTIME_SSH_TARGET)\n", color.YellowString("⚠"))
		}

		if !wildcardModeEnabled() {
			step("Traefik", "Verifying public routers via Traefik API")
			verified, err := verifyTraefikPublicRouters(ctx, traefikClient, app, base)
			if err != nil {
				return nil, nil, fmt.Errorf("traefik router verification: %w", err)
			}
			if verified {
				ok("Traefik", "public routers present in runtime API")
			}
		}
	}

	step("Coolify", "Triggering deployment")
	if err := coolifyClient.Deploy(ctx, appID); err != nil {
		return nil, nil, fmt.Errorf("coolify deploy: %w", err)
	}
	// Brief pause so Coolify transitions the status away from the previous
	// "running" container before we start polling — avoids a false-healthy
	// return while the new build is still in progress.
	time.Sleep(15 * time.Second)

	domainHint := app.GetDomains().Private
	if domainHint == "" {
		domainHint = app.GetDomains().Public
	}
	step("Health", fmt.Sprintf("Waiting for %s to become healthy", domainHint))
	if err := coolifyClient.WaitForHealthy(ctx, appID, 15*time.Minute); err != nil {
		return nil, nil, fmt.Errorf("health check failed: %w", err)
	}
	ok("Health", "service is healthy")

	if err := ensureCoolifyRuntimeContainer(ctx, coolifyClient, appID); err != nil {
		return nil, nil, fmt.Errorf("runtime container validation: %w", err)
	}
	if app.Spec.Type == "coolify-app" && app.GetDomains().Public != "" {
		step("Domain", "Verifying public DNS/TLS readiness")
		if err := verifyPublicDomainReadiness(app); err != nil {
			return nil, nil, err
		}
	}

	if app.Spec.Capabilities.Webhooks {
		githubToken := viper.GetString("GITHUB_TOKEN")
		if githubToken == "" {
			fmt.Printf("  %s [Webhooks] GITHUB_TOKEN not set — skipping webhook registration\n",
				color.YellowString("⚠"))
		} else {
			step("Webhooks", "Reconciling GitHub webhooks")
			ghClient := github.NewClient(githubToken)
			repo := github.RepoSlug(app.Spec.Repository.URL)
			allResources, err := coolifyClient.GetAppsByName(ctx, appName)
			if err != nil {
				fmt.Printf("  %s [Webhooks] could not list Coolify resources: %v\n",
					color.YellowString("⚠"), err)
			} else {
				for i := range allResources {
					resource := &allResources[i]
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

	domains := app.GetDomains()
	fmt.Printf("\n%s %s deployed successfully!\n", color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
	if domains.Private != "" {
		fmt.Printf("  Private: https://%s\n", domains.Private)
	}
	if domains.Public != "" {
		fmt.Printf("  Public: https://%s\n", domains.Public)
	}
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

	return app, loader, nil
}

func findVaultDependentApps(loader *manifest.Loader, sourceApp *manifest.AppConfig) ([]string, error) {
	if loader == nil || sourceApp == nil {
		return nil, nil
	}
	allApps, err := loader.LoadAll()
	if err != nil {
		return nil, err
	}
	sourcePath := sourceApp.VaultPath()
	dependents := make([]string, 0)
	for _, app := range allApps {
		if app == nil || app.Metadata.Name == sourceApp.Metadata.Name || !app.Spec.Enabled {
			continue
		}
		if appReferencesVaultPath(app, sourcePath) {
			dependents = append(dependents, app.Metadata.Name)
		}
	}
	return dependents, nil
}

func appReferencesVaultPath(app *manifest.AppConfig, sourcePath string) bool {
	for _, envVal := range app.Spec.Environment {
		matches := vaultRefPattern.FindAllStringSubmatch(envVal, -1)
		for _, match := range matches {
			if len(match) == 3 && match[1] == sourcePath {
				return true
			}
		}
	}
	for _, inject := range app.Spec.Secrets.Inject {
		if inject.VaultPath == sourcePath {
			return true
		}
	}
	return false
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

// runDatabaseBootstrap provisions the PostgreSQL database and role declared in
// app.Spec.Database, stores credentials in vault, and auto-patches the manifest
// file if secrets.inject is missing the DATABASE_URL entry.
//
// It is called from deploySingleApp when spec.database.enabled: true.
func runDatabaseBootstrap(
	ctx context.Context,
	app *manifest.AppConfig,
	base *manifest.BaseConfig,
	loader *manifest.Loader,
	vc *vault.Client,
) error {
	db := app.Spec.Database

	if !vc.Enabled() {
		fmt.Printf("  %s [Database] vault not configured — skipping DB bootstrap (set BAO_ADDR + BAO_TOKEN)\n",
			color.YellowString("⚠"))
		return nil
	}

	// ── Read admin credentials from vault ─────────────────────────────────
	adminPath := base.Database.AdminVaultPath
	if adminPath == "" {
		adminPath = "mn/data/lab/db01/admin/postgres-superuser" // hardcoded fallback
	}
	step("Database", fmt.Sprintf("Reading admin credentials from vault (%s)", adminPath))
	adminData, err := vc.KVRead(ctx, adminPath)
	if err != nil {
		return fmt.Errorf("read admin creds at %s: %w", adminPath, err)
	}
	if adminData == nil {
		return fmt.Errorf(
			"admin credentials not found at %s — seed them first:\n"+
				"  bao kv put %s DB_USER=postgres DB_PASSWORD=<pass>",
			adminPath, adminPath)
	}

	// Resolve host / port: app manifest overrides > vault admin data > base.yaml defaults.
	host := db.Host
	if host == "" {
		host = stringFromMap(adminData, "host", base.Database.DefaultHost)
	}
	if host == "" {
		return fmt.Errorf("database host is not configured — set base.yaml database.default_host or spec.database.host")
	}

	port := db.Port
	if port == 0 {
		port = intFromMap(adminData, "port", base.Database.DefaultPort)
	}
	if port == 0 {
		port = 5432
	}

	sslMode := db.SSLMode
	if sslMode == "" {
		sslMode = base.Database.DefaultSSLMode
	}
	if sslMode == "" {
		sslMode = "require"
	}

	adminUser := stringFromMap(adminData, "DB_USER", "postgres")
	adminPassword := stringFromMap(adminData, "DB_PASSWORD", "")
	if adminPassword == "" {
		return fmt.Errorf("DB_PASSWORD is empty at vault path %s", adminPath)
	}

	step("Database", fmt.Sprintf("Provisioning database %q / role %q on %s:%d", db.Name, db.Role, host, port))

	// ── Read existing app password (avoid rotation on re-deploy) ──────────
	// App DB credentials are stored at mn/data/lab/db01/apps/<name> (separate
	// from the general app vault path which holds OIDC + other credentials).
	dbAppsPath := base.Database.AppsVaultPath
	if dbAppsPath == "" {
		dbAppsPath = "mn/data/lab/db01/apps"
	}
	dbAppsPath = dbAppsPath + "/" + app.Metadata.Name

	existingPassword := ""
	existingSecrets, _ := vc.KVRead(ctx, dbAppsPath)
	if existingSecrets != nil {
		existingPassword, _ = existingSecrets["DB_PASSWORD"].(string)
	}

	// ── SSH Tunnel (if running from outside the lab network) ──────────────
	connHost, connPort := host, port
	if base.Database.SSHTunnel.Enabled {
		tunnelCfg := database.TunnelConfig{
			Enabled: true,
			Host:    base.Database.SSHTunnel.Host,
			Port:    base.Database.SSHTunnel.Port,
			User:    base.Database.SSHTunnel.User,
			KeyPath: base.Database.SSHTunnel.KeyPath,
		}
		remoteHost := base.Database.SSHTunnel.RemoteHost
		if remoteHost == "" {
			remoteHost = "localhost"
		}
		remotePort := base.Database.SSHTunnel.RemotePort
		if remotePort == 0 {
			remotePort = port
		}
		step("Database", fmt.Sprintf("Opening SSH tunnel via %s → %s:%d", tunnelCfg.Host, remoteHost, remotePort))
		tunnel, err := database.OpenTunnel(ctx, tunnelCfg, remoteHost, remotePort)
		if err != nil {
			return fmt.Errorf("database ssh tunnel: %w", err)
		}
		defer tunnel.Close()
		connHost, connPort = tunnel.LocalAddr()
		ok("Database", fmt.Sprintf("SSH tunnel established (local → %s:%d)", connHost, connPort))
	}

	// ── Provision ─────────────────────────────────────────────────────────
	dbCfg := database.Config{
		AdminHost:     host,
		AdminPort:     port,
		ConnHost:      connHost,
		ConnPort:      connPort,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
		DatabaseName:  db.Name,
		Role:          db.Role,
		Extensions:    db.Extensions,
		SSLMode:       sslMode,
		ReadonlyRoles: base.Database.ReadonlyRoles,
	}

	result, err := database.EnsureDatabase(ctx, dbCfg, existingPassword)
	if err != nil {
		return err
	}

	action := "verified"
	if result.Created {
		action = "created"
	} else if result.Rotated {
		action = "rotated"
	}
	ok("Database", fmt.Sprintf("role %q / database %q %s on %s:%d", db.Role, db.Name, action, host, port))

	// ── Write credentials to vault ────────────────────────────────────────
	// Key names match the convention already in use at mn/data/lab/db01/apps/*:
	// DATABASE_URL, DB_HOST, DB_PORT, DB_NAME, DB_USER, DB_PASSWORD.
	vaultData := map[string]string{
		"DATABASE_URL": result.Credentials.URL,
		"DB_HOST":      result.Credentials.Host,
		"DB_PORT":      fmt.Sprintf("%d", result.Credentials.Port),
		"DB_NAME":      result.Credentials.DatabaseName,
		"DB_USER":      result.Credentials.User,
		"DB_PASSWORD":  result.Credentials.Password,
	}
	step("Vault", fmt.Sprintf("Saving database credentials → %s", dbAppsPath))
	if err := vc.KVWrite(ctx, dbAppsPath, vaultData); err != nil {
		return fmt.Errorf("write database credentials to vault: %w", err)
	}
	ok("Vault", fmt.Sprintf("database credentials saved (%d keys)", len(vaultData)))

	// ── Set DATABASE_URL in memory for this deploy ─────────────────────────────
	// Use the freshly-provisioned credentials directly rather than re-reading
	// from vault. PatchSecrets (below) records a ${vault:...} ref in the manifest.
	if app.Spec.Environment == nil {
		app.Spec.Environment = make(manifest.Env)
	}
	if _, exists := app.Spec.Environment["DATABASE_URL"]; !exists {
		app.Spec.Environment["DATABASE_URL"] = result.Credentials.URL
	}

	// ── Auto-patch manifest file on disk ──────────────────────────────────
	// Writes DATABASE_URL: "${vault:dbAppsPath#DATABASE_URL}" into spec.environment
	// when missing, so subsequent deploys resolve it via the standard vault ref pass.
	patched, err := loader.PatchSecrets(app.Metadata.Name, app.EffectiveVaultPath(), dbAppsPath)
	if err != nil {
		fmt.Printf("  %s [Database] manifest auto-patch failed (non-fatal): %v\n",
			color.YellowString("⚠"), err)
	} else if patched {
		ok("Database", fmt.Sprintf("manifest auto-patched: DATABASE_URL env ref added to %s",
			loader.AppFilePath(app.Metadata.Name)))
	}

	return nil
}

// stringFromMap retrieves a string value from a map[string]interface{},
// returning the fallback when the key is absent or the value is not a string.
func stringFromMap(m map[string]interface{}, key, fallback string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// intFromMap retrieves an int value from a map[string]interface{},
// returning the fallback when the key is absent or unparseable.
func intFromMap(m map[string]interface{}, key string, fallback int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			if n != 0 {
				return n
			}
		case float64:
			if n != 0 {
				return int(n)
			}
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil && i != 0 {
				return i
			}
		}
	}
	return fallback
}
