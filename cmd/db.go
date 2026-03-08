package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/database"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/vault"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Database provisioning utilities",
	Long:  `Commands for managing PostgreSQL databases declared in app manifests.`,
}

var dbBootstrapCmd = &cobra.Command{
	Use:   "bootstrap <app-name>",
	Short: "Provision the PostgreSQL database and role for an app (no deploy)",
	Long: `Run the database bootstrap step in isolation — no Coolify, no Authentik, no deploy.

Reads the app manifest, connects to PostgreSQL as admin (credentials from vault),
creates the login role and database if they don't exist, generates or reuses a
strong password, stores credentials in vault, and auto-patches the manifest file
if the secrets.inject entry for DATABASE_URL is missing.

Use this to:
  - Test DB provisioning before a full deploy
  - Re-run provisioning after a DB was dropped or role was deleted
  - Verify vault credentials are up-to-date

Examples:
  mn-cli db bootstrap internal-api
  mn-cli db bootstrap api --stage prod
  mn-cli db bootstrap garmin-sync --rotate`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		appName := args[0]

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

		applyStage(app, dbBootstrapStage)

		if !app.Spec.Database.Enabled {
			return fmt.Errorf(
				"%s does not have spec.database.enabled: true — add the database block to the manifest first",
				appName)
		}

		vc := vault.NewClient(
			viper.GetString("BAO_ADDR"),
			viper.GetString("BAO_TOKEN"),
			viper.GetString("BAO_NAMESPACE"),
		)
		if !vc.Enabled() {
			return fmt.Errorf("vault not configured — set BAO_ADDR + BAO_TOKEN in .env or environment")
		}

		// If --rotate is requested, wipe the stored password so a new one is generated.
		if dbRotate {
			dbAppsPath := base.Database.AppsVaultPath
			if dbAppsPath == "" {
				dbAppsPath = "mn/data/lab/db01/apps"
			}
			dbAppsPath = dbAppsPath + "/" + appName
			step("Database", "Rotating password — erasing existing DB_PASSWORD from vault")
			existing, _ := vc.KVRead(ctx, dbAppsPath)
			if existing != nil {
				delete(existing, "DB_PASSWORD")
				clean := make(map[string]string, len(existing))
				for k, v := range existing {
					clean[k] = fmt.Sprintf("%v", v)
				}
				if err := vc.KVWrite(ctx, dbAppsPath, clean); err != nil {
					return fmt.Errorf("clearing DB_PASSWORD from vault: %w", err)
				}
				ok("Database", "DB_PASSWORD cleared — fresh password will be generated")
			}
		}

		fmt.Printf("%s bootstrapping database for %s...\n\n",
			color.CyanString("→"), color.New(color.Bold).Sprint(appName))

		if err := runDatabaseBootstrap(ctx, app, base, loader, vc); err != nil {
			return err
		}

		fmt.Printf("\n%s Database bootstrap complete for %s\n",
			color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
		fmt.Printf("\nTo verify, run:\n")
		fmt.Printf("  %s\n", color.CyanString("mn-cli db show %s", appName))
		fmt.Printf("\nTo use immediately in a deploy (credentials already in vault):\n")
		fmt.Printf("  %s\n", color.CyanString("mn-cli deploy %s", appName))

		return nil
	},
}

var dbShowCmd = &cobra.Command{
	Use:   "show <app-name>",
	Short: "Show database credentials and connection info from vault",
	Long: `Read and display the database credentials stored in vault for an app.
Does not connect to PostgreSQL — only reads from vault.

Examples:
  mn-cli db show internal-api
  mn-cli db show api`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		appName := args[0]

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

		if !app.Spec.Database.Enabled {
			fmt.Printf("%s %s does not have spec.database.enabled: true\n",
				color.YellowString("⚠"), appName)
			return nil
		}

		vc := vault.NewClient(
			viper.GetString("BAO_ADDR"),
			viper.GetString("BAO_TOKEN"),
			viper.GetString("BAO_NAMESPACE"),
		)
		if !vc.Enabled() {
			return fmt.Errorf("vault not configured — set BAO_ADDR + BAO_TOKEN")
		}

		dbAppsPath := base.Database.AppsVaultPath
		if dbAppsPath == "" {
			dbAppsPath = "mn/data/lab/db01/apps"
		}
		dbAppsPath = dbAppsPath + "/" + appName

		data, err := vc.KVRead(ctx, dbAppsPath)
		if err != nil {
			return fmt.Errorf("read vault path %s: %w", dbAppsPath, err)
		}

		fmt.Printf("\n%s Database info for %s\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName))
		fmt.Printf("  Vault path: %s\n\n", color.New(color.Faint).Sprint(dbAppsPath))

		if data == nil {
			fmt.Printf("  %s No database credentials found at vault path.\n"+
				"       Run: %s\n",
				color.YellowString("⚠"),
				color.CyanString("mn-cli db bootstrap %s", appName))
			return nil
		}

		keys := []string{"DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DATABASE_URL"}
		for _, k := range keys {
			v, ok := data[k]
			if !ok {
				continue
			}
			label := color.New(color.Bold).Sprintf("%-20s", k)
			val := fmt.Sprintf("%v", v)
			if k == "DATABASE_URL" {
				val = redactURL(val)
			}
			fmt.Printf("  %s %s\n", label, val)
		}

		if _, hasPwd := data["DB_PASSWORD"]; hasPwd {
			label := color.New(color.Bold).Sprintf("%-20s", "DB_PASSWORD")
			fmt.Printf("  %s %s\n", label, color.New(color.Faint).Sprint("<set — use --show-secret to reveal>"))
		} else {
			fmt.Printf("  %s %s\n",
				color.New(color.Bold).Sprintf("%-20s", "DB_PASSWORD"),
				color.YellowString("not set — run mn-cli db bootstrap %s", appName))
		}

		if showSecret {
			if pwd, ok := data["DB_PASSWORD"]; ok {
				fmt.Printf("\n  %s\n", color.RedString("⚠ DB_PASSWORD (sensitive):"))
				fmt.Printf("  %v\n", pwd)
			}
		}

		fmt.Printf("\n  Manifest config:\n")
		db := app.Spec.Database
		fmt.Printf("    role:        %s\n", db.Role)
		fmt.Printf("    database:    %s\n", db.Name)
		if len(db.Extensions) > 0 {
			fmt.Printf("    extensions:  %v\n", db.Extensions)
		}
		fmt.Printf("    ssl_mode:    %s\n", db.SSLMode)
		fmt.Println()

		return nil
	},
}

var (
	dbBootstrapStage string
	dbRotate         bool
	showSecret       bool
	dbForce          bool
)

// setupDBAdminConfig reads admin vault credentials and optionally opens an SSH
// tunnel. It returns a database.Config ready for use and a cleanup function
// (close the tunnel). Always call cleanup even on error (it is a no-op when the
// tunnel was never opened).
func setupDBAdminConfig(
	ctx context.Context,
	app *manifest.AppConfig,
	base *manifest.BaseConfig,
	vc *vault.Client,
) (database.Config, func(), error) {
	cleanup := func() {}

	adminPath := base.Database.AdminVaultPath
	if adminPath == "" {
		adminPath = "mn/data/lab/db01/admin/postgres-superuser"
	}
	adminData, err := vc.KVRead(ctx, adminPath)
	if err != nil {
		return database.Config{}, cleanup, fmt.Errorf("read admin creds at %s: %w", adminPath, err)
	}
	if adminData == nil {
		return database.Config{}, cleanup, fmt.Errorf(
			"admin credentials not found at %s — seed them first:\n"+
				"  bao kv put %s DB_USER=postgres DB_PASSWORD=<pass>",
			adminPath, adminPath)
	}

	db := app.Spec.Database
	host := db.Host
	if host == "" {
		host = stringFromMap(adminData, "host", base.Database.DefaultHost)
	}
	if host == "" {
		return database.Config{}, cleanup, fmt.Errorf(
			"database host not configured — set base.yaml database.default_host or spec.database.host")
	}
	port := db.Port
	if port == 0 {
		port = intFromMap(adminData, "port", base.Database.DefaultPort)
	}
	if port == 0 {
		port = 5432
	}
	adminUser := stringFromMap(adminData, "DB_USER", "postgres")
	adminPassword := stringFromMap(adminData, "DB_PASSWORD", "")
	if adminPassword == "" {
		return database.Config{}, cleanup, fmt.Errorf("DB_PASSWORD is empty at vault path %s", adminPath)
	}

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
			return database.Config{}, cleanup, fmt.Errorf("database ssh tunnel: %w", err)
		}
		connHost, connPort = tunnel.LocalAddr()
		cleanup = func() { tunnel.Close() }
		ok("Database", fmt.Sprintf("SSH tunnel established (local → %s:%d)", connHost, connPort))
	}

	cfg := database.Config{
		AdminHost:     host,
		AdminPort:     port,
		ConnHost:      connHost,
		ConnPort:      connPort,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
		DatabaseName:  app.Spec.Database.Name,
		Role:          app.Spec.Database.Role,
	}
	return cfg, cleanup, nil
}

// confirmDestructive prompts the user to type y/yes unless --force is set.
func confirmDestructive(prompt string) bool {
	if dbForce {
		return true
	}
	fmt.Print(prompt + " [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
}

// dropAppDatabase drops the PostgreSQL database + role and deletes the vault
// secret for the given app. It is shared between dbDropCmd, dbRecreateCmd,
// and the undeploy --drop-db flag.
func dropAppDatabase(ctx context.Context, app *manifest.AppConfig, base *manifest.BaseConfig, vc *vault.Client) error {
	dbName := app.Spec.Database.Name
	roleName := app.Spec.Database.Role

	step("Database", fmt.Sprintf("Dropping database %q and role %q", dbName, roleName))
	cfg, cleanup, err := setupDBAdminConfig(ctx, app, base, vc)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := database.DropDatabase(ctx, cfg); err != nil {
		return err
	}
	ok("Database", fmt.Sprintf("database %q and role %q dropped", dbName, roleName))

	dbAppsPath := base.Database.AppsVaultPath
	if dbAppsPath == "" {
		dbAppsPath = "mn/data/lab/db01/apps"
	}
	dbAppsPath = dbAppsPath + "/" + app.Metadata.Name

	step("Vault", fmt.Sprintf("Deleting credentials at %s", dbAppsPath))
	if err := vc.KVDelete(ctx, dbAppsPath); err != nil {
		fmt.Printf("  %s [Vault] delete failed (non-fatal): %v\n", color.YellowString("⚠"), err)
	} else {
		ok("Vault", "credentials deleted")
	}
	return nil
}

var dbDropCmd = &cobra.Command{
	Use:   "drop <app-name>",
	Short: "Drop the PostgreSQL database and role for an app (irreversible)",
	Long: `Terminate all active connections, drop the PostgreSQL database and login
role declared in the app manifest, and delete the vault secret at the app's
database vault path.

WARNING: All data in the database is permanently deleted. This cannot be undone.

Examples:
  mn-cli db drop hello-world-internal
  mn-cli db drop hello-world-internal --force`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		appName := args[0]

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

		if !app.Spec.Database.Enabled {
			return fmt.Errorf("%s does not have spec.database.enabled: true", appName)
		}

		vc := vault.NewClient(
			viper.GetString("BAO_ADDR"),
			viper.GetString("BAO_TOKEN"),
			viper.GetString("BAO_NAMESPACE"),
		)
		if !vc.Enabled() {
			return fmt.Errorf("vault not configured — set BAO_ADDR + BAO_TOKEN")
		}

		dbName := app.Spec.Database.Name
		roleName := app.Spec.Database.Role

		fmt.Printf("\n  %s [Database] This will permanently delete:\n", color.RedString("⚠"))
		fmt.Printf("      Database: %s\n", dbName)
		fmt.Printf("      Role:     %s\n", roleName)
		fmt.Printf("      Vault:    %s/%s\n\n",
			func() string {
				p := base.Database.AppsVaultPath
				if p == "" {
					p = "mn/data/lab/db01/apps"
				}
				return p
			}(), appName)

		if !confirmDestructive("  All data will be lost. Continue?") {
			fmt.Println("  Aborted.")
			return nil
		}

		if err := dropAppDatabase(ctx, app, base, vc); err != nil {
			return err
		}

		fmt.Printf("\n%s Database for %s dropped. Run '%s' to re-provision.\n",
			color.GreenString("✓"), color.New(color.Bold).Sprint(appName),
			color.CyanString("mn-cli db bootstrap %s", appName))
		return nil
	},
}

var dbRecreateCmd = &cobra.Command{
	Use:   "recreate <app-name>",
	Short: "Drop and re-provision the PostgreSQL database and role for an app",
	Long: `Drop the existing PostgreSQL database and role, then run database bootstrap
to provision fresh ones with new credentials.

Equivalent to: mn-cli db drop <app> && mn-cli db bootstrap <app>

A new password is always generated (credentials are wiped before re-provisioning).

Examples:
  mn-cli db recreate hello-world-internal
  mn-cli db recreate hello-world-internal --force`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		appName := args[0]

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

		if !app.Spec.Database.Enabled {
			return fmt.Errorf("%s does not have spec.database.enabled: true", appName)
		}

		vc := vault.NewClient(
			viper.GetString("BAO_ADDR"),
			viper.GetString("BAO_TOKEN"),
			viper.GetString("BAO_NAMESPACE"),
		)
		if !vc.Enabled() {
			return fmt.Errorf("vault not configured — set BAO_ADDR + BAO_TOKEN")
		}

		dbName := app.Spec.Database.Name
		roleName := app.Spec.Database.Role

		fmt.Printf("\n  %s [Database] This will DROP and re-create:\n", color.YellowString("⚠"))
		fmt.Printf("      Database: %s\n", dbName)
		fmt.Printf("      Role:     %s\n", roleName)
		fmt.Printf("      A new password will be generated.\n\n")

		if !confirmDestructive("  All existing data will be lost. Continue?") {
			fmt.Println("  Aborted.")
			return nil
		}

		// ── Drop phase ───────────────────────────────────────────────────────
		fmt.Printf("\n%s dropping existing database for %s...\n\n",
			color.CyanString("→"), color.New(color.Bold).Sprint(appName))

		cfg, cleanup, err := setupDBAdminConfig(ctx, app, base, vc)
		if err != nil {
			return err
		}

		step("Database", fmt.Sprintf("Dropping database %q and role %q", dbName, roleName))
		if err := database.DropDatabase(ctx, cfg); err != nil {
			cleanup()
			return err
		}
		ok("Database", fmt.Sprintf("database %q and role %q dropped", dbName, roleName))
		cleanup() // close tunnel; bootstrap will open a fresh one if needed

		// Wipe vault credentials so a fresh password is generated on bootstrap.
		dbAppsPath := base.Database.AppsVaultPath
		if dbAppsPath == "" {
			dbAppsPath = "mn/data/lab/db01/apps"
		}
		dbAppsPath = dbAppsPath + "/" + appName

		step("Vault", fmt.Sprintf("Wiping existing credentials at %s", dbAppsPath))
		if err := vc.KVDelete(ctx, dbAppsPath); err != nil {
			fmt.Printf("  %s [Vault] delete failed (non-fatal): %v\n", color.YellowString("⚠"), err)
		} else {
			ok("Vault", "credentials wiped — fresh password will be generated")
		}

		// ── Bootstrap phase ──────────────────────────────────────────────────
		fmt.Printf("\n%s re-provisioning database for %s...\n\n",
			color.CyanString("→"), color.New(color.Bold).Sprint(appName))

		if err := runDatabaseBootstrap(ctx, app, base, loader, vc); err != nil {
			return err
		}

		fmt.Printf("\n%s Database recreated for %s\n",
			color.GreenString("✓"), color.New(color.Bold).Sprint(appName))
		fmt.Printf("\nTo verify, run:\n")
		fmt.Printf("  %s\n", color.CyanString("mn-cli db show %s", appName))

		return nil
	},
}

func init() {
	dbBootstrapCmd.Flags().StringVar(&dbBootstrapStage, "stage", "dev", "Stage: dev or prod")
	dbBootstrapCmd.Flags().BoolVar(&dbRotate, "rotate", false, "Force password rotation (generates a new password)")
	dbShowCmd.Flags().BoolVar(&showSecret, "show-secret", false, "Print the raw password (sensitive)")
	dbDropCmd.Flags().BoolVar(&dbForce, "force", false, "Skip confirmation prompt")
	dbRecreateCmd.Flags().BoolVar(&dbForce, "force", false, "Skip confirmation prompt")

	dbCmd.AddCommand(dbBootstrapCmd)
	dbCmd.AddCommand(dbShowCmd)
	dbCmd.AddCommand(dbDropCmd)
	dbCmd.AddCommand(dbRecreateCmd)
	rootCmd.AddCommand(dbCmd)
}

// redactURL replaces the password in a postgresql:// URL with ***
func redactURL(rawURL string) string {
	// postgresql://user:pass@host:port/db?...
	// Find :// then scan to @ replacing what's between : and @
	const prefix = "://"
	idx := len("postgresql") + len(prefix)
	if len(rawURL) <= idx {
		return rawURL
	}
	rest := rawURL[idx:]
	atIdx := -1
	colonIdx := -1
	for i, ch := range rest {
		if ch == ':' && colonIdx == -1 {
			colonIdx = i
		}
		if ch == '@' {
			atIdx = i
			break
		}
	}
	if colonIdx < 0 || atIdx < 0 || colonIdx >= atIdx {
		return rawURL
	}
	return rawURL[:idx+colonIdx+1] + "***" + rawURL[idx+atIdx:]
}
