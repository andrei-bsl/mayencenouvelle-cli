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

var deleteApp bool

var undeployCmd = &cobra.Command{
	Use:   "undeploy <app-name>",
	Short: "Stop (or permanently delete) a deployed app",
	Long: `Stops a running Coolify application.

By default, only the container is stopped — the app config, env vars, and
git settings remain in Coolify so it can be re-deployed at any time.

With --delete, the application is permanently removed from Coolify.
A subsequent 'deploy' will recreate it from scratch (including the manual
domain setup step in the Coolify UI).

Examples:
  mayence undeploy hello-world            Stop the container (config preserved)
  mayence undeploy hello-world --delete   Permanently remove from Coolify`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		appName := args[0]

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Load manifest to check auth capability
		loader, err := manifest.NewLoader(manifestsDir)
		if err != nil {
			return fmt.Errorf("loading manifests: %w", err)
		}
		app, err := loader.LoadApp(appName)
		if err != nil {
			return err
		}

		coolifyClient := coolify.NewClient(
			viper.GetString("COOLIFY_URL"),
			viper.GetString("COOLIFY_API_TOKEN"),
		)

		// Resolve UUID by name
		svc, err := coolifyClient.GetAppByName(ctx, appName)
		if err != nil {
			return fmt.Errorf("coolify: %w", err)
		}
		if svc == nil {
			fmt.Printf("%s %s is not deployed in Coolify (nothing to do)\n",
				color.YellowString("⚠"), appName)
			return nil
		}

		if deleteApp {
			// Collect webhook URLs from ALL Coolify resources BEFORE deleting,
			// so we can clean up GitHub webhooks after the Coolify resource is gone.
			var webhookURLs []string
			if app.Spec.Capabilities.Webhooks {
				if allResources, err := coolifyClient.GetAppsByName(ctx, appName); err == nil {
					for _, r := range allResources {
						if r.ManualWebhookSecretGithub != "" {
							webhookURLs = append(webhookURLs, coolifyClient.WebhookURL(r.ManualWebhookSecretGithub))
						}
					}
				}
			}

			fmt.Printf("%s This will %s %s from Coolify (uuid: %s).\n",
				color.YellowString("⚠"),
				color.New(color.Bold, color.FgRed).Sprint("permanently delete"),
				color.New(color.Bold).Sprint(appName),
				svc.UUID,
			)
			if app.Spec.Capabilities.Auth == "oidc" {
				fmt.Printf("  Authentik OAuth2 provider (%s) and application (%s) will also be removed.\n",
					app.ProviderName(), app.AppSlug())
			}
			if len(webhookURLs) > 0 {
				fmt.Printf("  %d GitHub webhook(s) will also be removed from %s.\n",
					len(webhookURLs), github.RepoSlug(app.Spec.Repository.URL))
			}
			fmt.Printf("  Press Ctrl+C within 5s to abort...")
			time.Sleep(5 * time.Second)
			fmt.Println()

			if app.Spec.Capabilities.Auth == "oidc" {
				step("Authentik", fmt.Sprintf("Removing provider %s and application %s", app.ProviderName(), app.AppSlug()))
				authentikClient := authentik.NewClient(
					viper.GetString("AUTHENTIK_URL"),
					viper.GetString("AUTHENTIK_API_TOKEN"),
				)
				if err := authentikClient.DeleteOIDC(ctx, appName); err != nil {
					return fmt.Errorf("authentik delete: %w", err)
				}
				ok("Authentik", "OAuth2 provider and application removed")
			}

			step("Coolify", fmt.Sprintf("Deleting %s", appName))
			if err := coolifyClient.Delete(ctx, svc.UUID); err != nil {
				return fmt.Errorf("coolify delete: %w", err)
			}
			ok("Coolify", fmt.Sprintf("%s deleted from Coolify", appName))

			// Remove GitHub webhooks (after Coolify delete — order doesn't matter for GitHub)
			if len(webhookURLs) > 0 {
				githubToken := viper.GetString("GITHUB_TOKEN")
				if githubToken != "" {
					step("Webhooks", "Removing GitHub webhooks")
					ghClient := github.NewClient(githubToken)
					repoSlug := github.RepoSlug(app.Spec.Repository.URL)
					for _, wURL := range webhookURLs {
						if err := ghClient.DeleteWebhookByURL(ctx, repoSlug, wURL); err != nil {
							fmt.Printf("  %s [Webhooks] delete skipped: %v\n", color.YellowString("⚠"), err)
						} else {
							ok("Webhooks", fmt.Sprintf("removed %s", wURL))
						}
					}
				} else {
					fmt.Printf("  %s [Webhooks] GITHUB_TOKEN not set — webhooks not removed from GitHub\n",
						color.YellowString("⚠"))
				}
			}

			fmt.Printf("\n%s %s deleted. Run 'mn-cli deploy %s' to redeploy from scratch.\n",
				color.GreenString("✓"), color.New(color.Bold).Sprint(appName), appName)
			return nil
		}

		// Default: stop only
		fmt.Printf("%s stopping %s...\n\n", color.CyanString("→"), color.New(color.Bold).Sprint(appName))
		step("Coolify", fmt.Sprintf("Stopping %s (uuid: %s)", appName, svc.UUID))
		if err := coolifyClient.Stop(ctx, svc.UUID); err != nil {
			return fmt.Errorf("coolify stop: %w", err)
		}
		ok("Coolify", "stop request accepted")

		fmt.Printf("\n%s %s stopped. Config preserved in Coolify — run 'mn-cli deploy %s' to restart.\n",
			color.GreenString("✓"), color.New(color.Bold).Sprint(appName), appName)
		return nil
	},
}

func init() {
	undeployCmd.Flags().BoolVar(&deleteApp, "delete", false, "Permanently delete the app from Coolify (irreversible)")
}
