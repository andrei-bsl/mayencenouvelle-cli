package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version   string
	buildDate string

	cfgFile      string
	manifestsDir string
	dryRun       bool
	verbose      bool
)

// rootCmd is the base command for the mayence CLI.
var rootCmd = &cobra.Command{
	Use:   "mayence",
	Short: "Mayence Nouvelle Lab orchestration CLI",
	Long: `mayence automates app deployment, authentication provisioning,
and infrastructure configuration across the Mayence Nouvelle homelab.

It reads declarative manifests from workspace/manifests/apps/ and applies
changes to Coolify, Authentik, Traefik, and DNS (AdGuard Home).

Examples:
  mayence validate                  Validate all app manifests
  mayence list apps                 Show all defined apps with status
  mayence status                    Health check all deployed services
  mayence plan nas-app              Preview changes without applying
  mayence deploy nas-app            Deploy a single app end-to-end
  mayence apply-manifest            Deploy all enabled apps in dependency order
  mayence rotate-secret nas-app     Rotate Authentik OAuth2 credentials
  mayence rollback nas-app          Roll back to previous deployment
`,
	SilenceUsage: true,
}

// SetVersion sets version and build date (called from main.go).
func SetVersion(v, d string) {
	version = v
	buildDate = d
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Persistent flags available to all subcommands
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "Config file (default: $HOME/.mayence.yaml)")
	rootCmd.PersistentFlags().StringVar(&manifestsDir, "manifests", "./workspace/manifests", "Path to manifests directory")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Preview changes without applying (same as 'plan')")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Register subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(planCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(applyCmd)
	rootCmd.AddCommand(undeployCmd)
	rootCmd.AddCommand(rotateCmd)
	rootCmd.AddCommand(rollbackCmd)
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".mayence")
	}

	viper.AutomaticEnv() // Read from environment variables

	if err := viper.ReadInConfig(); err == nil && verbose {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}

// versionCmd prints the CLI version.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the mayence CLI version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("mayence %s (built %s)\n", version, buildDate)
	},
}
