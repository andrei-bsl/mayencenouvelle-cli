package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List apps and infrastructure defined in manifests",
}

var listAppsCmd = &cobra.Command{
	Use:   "apps",
	Short: "List all app manifests with their type, zone, and enabled status",
	Long: `Lists all app manifests loaded from workspace/manifests/apps/.
Annotates each with: type, zone exposure, auth strategy, enabled status.

Examples:
  mayence list apps
  mayence list apps --all     Include disabled apps`,
	RunE: func(cmd *cobra.Command, args []string) error {
		showAll, _ := cmd.Flags().GetBool("all")

		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}

		apps, err := loader.LoadAll()
		if err != nil {
			return err
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tEXPOSURE\tAUTH\tPHASE\tSTATUS")
		fmt.Fprintln(w, "----\t----\t--------\t----\t-----\t------")

		for _, app := range apps {
			if !showAll && !app.Spec.Enabled {
				continue
			}

			status := color.GreenString("enabled")
			if !app.Spec.Enabled {
				status = color.YellowString("disabled")
			}

			exposure := app.Spec.Capabilities.Exposure
			auth := app.Spec.Capabilities.Auth
			phase := app.Metadata.Phase

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				app.Metadata.Name,
				app.Spec.Type,
				exposure,
				auth,
				phase,
				status,
			)
		}
		w.Flush()
		return nil
	},
}

func init() {
	listAppsCmd.Flags().Bool("all", false, "Include disabled apps")
	listCmd.AddCommand(listAppsCmd)
}
