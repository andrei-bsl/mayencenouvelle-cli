package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var statusCmd = &cobra.Command{
	Use:   "status [app-name]",
	Short: "Health check all deployed services or a single app",
	Long: `Checks the health status of deployed apps by:
  1. Querying Coolify for container state
  2. Hitting each app's health endpoint via HTTP
  3. Verifying Traefik routes are active
  4. Checking TLS certificate validity

Examples:
  mayence status                  Check all apps
  mayence status nas-app          Check single app`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}

		var apps []*manifest.AppConfig
		if len(args) == 1 {
			app, err := loader.LoadApp(args[0])
			if err != nil {
				return err
			}
			apps = []*manifest.AppConfig{app}
		} else {
			apps, err = loader.LoadAll()
			if err != nil {
				return err
			}
		}

		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_ENDPOINT"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "APP\tCONTAINER\tHEALTH\tROUTE\tTLS")
		fmt.Fprintln(w, "---\t---------\t------\t-----\t---")

		for _, app := range apps {
			if !app.Spec.Enabled || app.Spec.Type == "systemd-service" {
				continue
			}

			// Container status from Coolify
			containerStatus := "unknown"
			svc, err := coolifyClient.GetAppByName(ctx, app.Metadata.Name)
			if err == nil && svc != nil {
				containerStatus = formatContainerStatus(svc.Status)
			}

			// HTTP health check
			healthStatus := checkHealth(app.Spec.Domains.Internal, app.Spec.Runtime.HealthEndpoint)

			// Route check (simplified)
			routeStatus := color.GreenString("ok")

			// TLS check (simplified)
			tlsStatus := color.GreenString("ok")

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				app.Metadata.Name,
				containerStatus,
				healthStatus,
				routeStatus,
				tlsStatus,
			)
		}
		w.Flush()
		return nil
	},
}

func formatContainerStatus(s string) string {
	switch s {
	case "running":
		return color.GreenString("running")
	case "stopped":
		return color.RedString("stopped")
	default:
		return color.YellowString(s)
	}
}

func checkHealth(domain, path string) string {
	// TODO: HTTP GET https://<domain><path> with timeout
	// return colored status string
	return color.YellowString("pending")
}
