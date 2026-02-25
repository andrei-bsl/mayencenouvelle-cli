package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/coolify"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/manifest"
	"github.com/mayencenouvelle/mayencenouvelle-cli/internal/traefik"
	"github.com/spf13/viper"
)

const defaultRuntimeTraefikDir = "/srv/docker/infra/traefik/dynamic"

func syncTraefikRuntimeFiles(localTraefikDir string) (bool, error) {
	sshTarget := strings.TrimSpace(viper.GetString("MN_TRAEFIK_RUNTIME_SSH_TARGET"))
	if sshTarget == "" {
		sshTarget = strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_TARGET"))
	}
	if sshTarget == "" {
		return false, nil
	}
	remoteDir := strings.TrimSpace(viper.GetString("MN_TRAEFIK_RUNTIME_DYNAMIC_DIR"))
	if remoteDir == "" {
		remoteDir = defaultRuntimeTraefikDir
	}

	files := []string{
		filepath.Join(localTraefikDir, "coolify-apps-public.yml"),
		filepath.Join(localTraefikDir, "coolify-apps-public-managed.yml"),
	}
	existing := make([]string, 0, len(files))
	for _, p := range files {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return false, fmt.Errorf("no traefik public router files found in %s", localTraefikDir)
	}

	// Ensure remote directory exists (andrei may not own /srv/docker/infra/traefik/dynamic,
	// so use sudo mkdir -p instead of install -d to avoid permission errors).
	mkdirCmd := fmt.Sprintf("sudo mkdir -p %s", remoteDir)
	if err := runLocalCommand(15*time.Second, "ssh", "-o", "BatchMode=yes", sshTarget, mkdirCmd); err != nil {
		return false, err
	}
	// Use "cat | ssh sudo tee" instead of scp: the target directory is root-owned and requires
	// sudo to write. scp cannot sudo, but piping through tee with sudo works cleanly.
	for _, src := range existing {
		remotePath := fmt.Sprintf("%s/%s", remoteDir, filepath.Base(src))
		content, err := os.ReadFile(src)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", src, err)
		}
		remoteCmd := fmt.Sprintf("sudo tee %s > /dev/null", remotePath)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", sshTarget, remoteCmd)
		cmd.Stdin = strings.NewReader(string(content))
		out, cmdErr := cmd.CombinedOutput()
		cancel()
		if cmdErr != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = cmdErr.Error()
			}
			return false, fmt.Errorf("syncing %s via ssh+sudo tee: %s", filepath.Base(src), msg)
		}
	}
	return true, nil
}

func verifyTraefikPublicRouters(ctx context.Context, tf *traefik.Client, app *manifest.AppConfig, base *manifest.BaseConfig) error {
	domains := app.GetDomains()
	hosts := splitHostCSV(domains.Public)
	if len(hosts) == 0 {
		return nil
	}

	apiURL := strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_URL"))
	if apiURL == "" && base != nil {
		apiURL = strings.TrimSpace(base.Traefik.AdminEndpoint)
	}
	if apiURL == "" {
		return fmt.Errorf("MN_TRAEFIK_API_URL (or base.traefik.admin_endpoint) is required to verify public routers")
	}

	insecure := strings.EqualFold(strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_INSECURE")), "true")
	missing, err := tf.MissingHostsFromAPI(ctx, apiURL, hosts, insecure)
	if err != nil {
		// Traefik API port (8080) is only accessible from within the lab network.
		// When running from a dev machine, connection refused or network unreachable
		// is expected — downgrade to a warning so the deploy still proceeds.
		if isNetworkUnreachable(err) {
			fmt.Printf("  %s [Traefik] API unreachable from this machine (%s) — skipping router verification\n",
				"\033[33m⚠\033[0m", apiURL)
			return nil
		}
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("traefik routers missing for host(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// isNetworkUnreachable returns true for errors that indicate the endpoint is not
// reachable from the current machine (connection refused, no route to host, timeout).
// These are expected when running mn-cli from outside the lab network.
func isNetworkUnreachable(err error) bool {
	s := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"connection refused",
		"no route to host",
		"network is unreachable",
		"i/o timeout",
		"context deadline exceeded",
		"dial tcp",
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

func ensureCoolifyRuntimeContainer(ctx context.Context, coolifyClient *coolify.Client, appUUID string) error {
	sshTarget := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_TARGET"))
	if sshTarget == "" {
		return nil
	}
	app, err := coolifyClient.GetApp(ctx, appUUID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(app.Status, "running") {
		return nil
	}

	exists, err := runtimeContainerExists(sshTarget, appUUID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	step("Coolify", fmt.Sprintf("container missing on runtime host while status=%s, forcing restart", app.Status))
	if err := coolifyClient.Restart(ctx, appUUID); err != nil {
		return err
	}
	if err := coolifyClient.WaitForHealthy(ctx, appUUID, 3*time.Minute); err != nil {
		return err
	}
	ok("Coolify", "restart completed and service is healthy")
	return nil
}

func runtimeContainerExists(sshTarget, appUUID string) (bool, error) {
	remoteCmd := fmt.Sprintf("sh -lc \"docker ps -a --format '{{.Names}}' | grep -F -- '%s' >/dev/null\"", appUUID)
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", sshTarget, remoteCmd)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.ExitStatus() == 1 {
			return false, nil
		}
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return false, fmt.Errorf("runtime container check failed: %s", msg)
}

func splitHostCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runLocalCommand(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}
