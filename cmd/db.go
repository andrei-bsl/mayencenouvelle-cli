package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/fatih/color"
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
			step("Database", "Rotating password — erasing existing database_password from vault")
			existing, _ := vc.KVRead(ctx, app.EffectiveVaultPath())
			if existing != nil {
				delete(existing, "database_password")
				clean := make(map[string]string, len(existing))
				for k, v := range existing {
					clean[k] = fmt.Sprintf("%v", v)
				}
				if err := vc.KVWrite(ctx, app.EffectiveVaultPath(), clean); err != nil {
					return fmt.Errorf("clearing database_password from vault: %w", err)
				}
				ok("Database", "database_password cleared — fresh password will be generated")
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

		vaultPath := app.EffectiveVaultPath()
		data, err := vc.KVRead(ctx, vaultPath)
		if err != nil {
			return fmt.Errorf("read vault path %s: %w", vaultPath, err)
		}

		fmt.Printf("\n%s Database info for %s\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName))
		fmt.Printf("  Vault path: %s\n\n", color.New(color.Faint).Sprint(vaultPath))

		if data == nil {
			fmt.Printf("  %s No database credentials found at vault path.\n"+
				"       Run: %s\n",
				color.YellowString("⚠"),
				color.CyanString("mn-cli db bootstrap %s", appName))
			return nil
		}

		keys := []string{"database_host", "database_port", "database_name", "database_user", "database_url"}
		for _, k := range keys {
			v, ok := data[k]
			if !ok {
				continue
			}
			label := color.New(color.Bold).Sprintf("%-20s", k)
			val := fmt.Sprintf("%v", v)
			// Redact password from URL but show the rest
			if k == "database_url" {
				val = redactURL(val)
			}
			fmt.Printf("  %s %s\n", label, val)
		}

		// Password: show only that it exists, not the value
		if _, hasPwd := data["database_password"]; hasPwd {
			label := color.New(color.Bold).Sprintf("%-20s", "database_password")
			fmt.Printf("  %s %s\n", label, color.New(color.Faint).Sprint("<set — use --show-secret to reveal>"))
		} else {
			fmt.Printf("  %s %s\n",
				color.New(color.Bold).Sprintf("%-20s", "database_password"),
				color.YellowString("not set — run mn-cli db bootstrap %s", appName))
		}

		if showSecret {
			if pwd, ok := data["database_password"]; ok {
				fmt.Printf("\n  %s\n", color.RedString("⚠ database_password (sensitive):"))
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
)

func init() {
	dbBootstrapCmd.Flags().StringVar(&dbBootstrapStage, "stage", "dev", "Stage: dev or prod")
	dbBootstrapCmd.Flags().BoolVar(&dbRotate, "rotate", false, "Force password rotation (generates a new password)")
	dbShowCmd.Flags().BoolVar(&showSecret, "show-secret", false, "Print the raw password (sensitive)")

	dbCmd.AddCommand(dbBootstrapCmd)
	dbCmd.AddCommand(dbShowCmd)
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
