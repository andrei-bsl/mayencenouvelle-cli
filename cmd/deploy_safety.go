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

	if err := runLocalCommand(15*time.Second, "ssh", "-o", "BatchMode=yes", sshTarget, "install", "-d", remoteDir); err != nil {
		return false, err
	}
	for _, src := range existing {
		dst := fmt.Sprintf("%s:%s/%s", sshTarget, remoteDir, filepath.Base(src))
		if err := runLocalCommand(20*time.Second, "scp", "-q", src, dst); err != nil {
			return false, err
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
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("traefik routers missing for host(s): %s", strings.Join(missing, ", "))
	}
	return nil
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
