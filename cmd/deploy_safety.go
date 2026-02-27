package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	"gopkg.in/yaml.v3"
)

const defaultRuntimeTraefikDir = "/srv/docker/infra/traefik/dynamic"

type staticPublicConfig struct {
	HTTP struct {
		Routers map[string]struct{} `yaml:"routers"`
	} `yaml:"http"`
}

func syncTraefikRuntimeFiles(localTraefikDir string) (bool, error) {
	sshTarget := resolveSSHTarget(true)
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
	sshArgs := append(sshBaseArgs(), sshTarget, mkdirCmd)
	if err := runLocalCommand(15*time.Second, "ssh", sshArgs...); err != nil {
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
		args := append(sshBaseArgs(), sshTarget, remoteCmd)
		cmd := exec.CommandContext(ctx, "ssh", args...)
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

func verifyTraefikPublicRouters(ctx context.Context, tf *traefik.Client, app *manifest.AppConfig, base *manifest.BaseConfig) (bool, error) {
	domains := app.GetDomains()
	hosts := splitHostCSV(domains.Public)
	if len(hosts) == 0 {
		return false, nil
	}

	apiURL := strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_URL"))
	if apiURL == "" && base != nil {
		apiURL = strings.TrimSpace(base.Traefik.AdminEndpoint)
	}
	if apiURL == "" {
		return false, fmt.Errorf("MN_TRAEFIK_API_URL (or base.traefik.admin_endpoint) is required to verify public routers")
	}
	apiURL = normalizeTraefikAPIURL(apiURL)

	insecure := strings.EqualFold(strings.TrimSpace(viper.GetString("MN_TRAEFIK_API_INSECURE")), "true")
	missing, err := tf.MissingHostsFromAPI(ctx, apiURL, hosts, insecure)
	if err != nil {
		// Common homelab case: self-signed internal cert not trusted by local OS trust store.
		// Retry once with TLS verify disabled so deploy does not fail on local trust issues.
		if !insecure && isCertAuthorityError(err) {
			fmt.Printf("  %s [Traefik] certificate not trusted locally for %s — retrying with insecure TLS\n",
				"\033[33m⚠\033[0m", apiURL)
			missing, err = tf.MissingHostsFromAPI(ctx, apiURL, hosts, true)
		}
	}
	if err != nil {
		// Traefik API port (8080) is only accessible from within the lab network.
		// When running from a dev machine, connection refused or network unreachable
		// is expected — downgrade to a warning so the deploy still proceeds.
		if isNetworkUnreachable(err) {
			fmt.Printf("  %s [Traefik] API unreachable from this machine (%s) — skipping router verification\n",
				"\033[33m⚠\033[0m", apiURL)
			return false, nil
		}
		return false, err
	}
	if len(missing) > 0 {
		return false, fmt.Errorf("traefik routers missing for host(s): %s", strings.Join(missing, ", "))
	}
	return true, nil
}

func normalizeTraefikAPIURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	path := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/dashboard"):
		u.Path = strings.TrimSuffix(path, "/dashboard")
	case strings.HasSuffix(path, "/dashboard/"):
		u.Path = strings.TrimSuffix(path, "/dashboard/")
	case strings.HasSuffix(path, "/dashboard.html"):
		u.Path = strings.TrimSuffix(path, "/dashboard.html")
	}
	if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/api/http/routers") {
		return strings.TrimRight(u.String(), "/")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/http/routers"
	return strings.TrimRight(u.String(), "/")
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

func isCertAuthorityError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "certificate signed by unknown authority") ||
		strings.Contains(s, "x509: ")
}

func ensureCoolifyRuntimeContainer(ctx context.Context, coolifyClient *coolify.Client, appUUID string) error {
	sshTarget := resolveSSHTarget(false)
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
	args := append(sshBaseArgs(), sshTarget, remoteCmd)
	cmd := exec.Command("ssh", args...)
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

func resolveSSHTarget(preferTraefik bool) string {
	if preferTraefik {
		if t := strings.TrimSpace(viper.GetString("MN_TRAEFIK_RUNTIME_SSH_TARGET")); t != "" {
			return t
		}
	}
	if t := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_TARGET")); t != "" {
		return t
	}
	host := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_HOST"))
	if host == "" {
		return ""
	}
	user := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_USER"))
	if user == "" {
		return host
	}
	return user + "@" + host
}

func sshBaseArgs() []string {
	args := []string{"-o", "BatchMode=yes"}
	if port := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_PORT")); port != "" {
		args = append(args, "-p", port)
	}
	if key := strings.TrimSpace(viper.GetString("MN_RUNTIME_SSH_KEY_FILE")); key != "" {
		args = append(args, "-i", key)
	}
	return args
}

func enforceSinglePublicRouterSource(localTraefikDir string) error {
	if strings.EqualFold(strings.TrimSpace(viper.GetString("MN_ALLOW_STATIC_PUBLIC_ROUTERS")), "true") {
		return nil
	}
	staticPath := filepath.Join(localTraefikDir, "coolify-apps-public.yml")
	data, err := os.ReadFile(staticPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading static public router file: %w", err)
	}
	var cfg staticPublicConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing static public router file: %w", err)
	}
	if len(cfg.HTTP.Routers) > 0 {
		return fmt.Errorf("static public router file still contains %d router(s): %s; move routers to managed file or set MN_ALLOW_STATIC_PUBLIC_ROUTERS=true temporarily", len(cfg.HTTP.Routers), staticPath)
	}
	return nil
}

func wildcardModeEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(viper.GetString("MN_TRAEFIK_PUBLIC_WILDCARD_ENABLED")), "true")
}

func verifyWildcardPublicRouter(ctx context.Context, baseURL string, insecure bool) error {
	baseURL = normalizeTraefikAPIURL(baseURL)
	suffix := strings.TrimSpace(viper.GetString("MN_TRAEFIK_PUBLIC_WILDCARD_SUFFIX"))
	if suffix == "" {
		suffix = "apps.mayencenouvelle.com"
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, // #nosec G402
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return fmt.Errorf("building traefik wildcard verification request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling traefik api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("traefik api returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading traefik api response: %w", err)
	}
	var routers []struct {
		Rule    string `json:"rule"`
		Service string `json:"service"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &routers); err != nil {
		return fmt.Errorf("parsing traefik routers response: %w", err)
	}
	for _, r := range routers {
		if r.Status != "enabled" {
			continue
		}
		if !strings.Contains(r.Rule, "HostRegexp(") {
			continue
		}
		if !strings.Contains(r.Rule, suffix) {
			continue
		}
		if strings.Contains(r.Service, "coolify-traefik-svc") {
			return nil
		}
	}
	return fmt.Errorf("no enabled wildcard public router found for suffix %q to coolify-traefik-svc", suffix)
}

// verifyPublicDomainReadiness performs post-deploy DNS/TLS/HTTP probes for
// public domains. In non-strict mode, probe failures are warnings.
func verifyPublicDomainReadiness(app *manifest.AppConfig) error {
	hosts := splitHostCSV(app.GetDomains().Public)
	if len(hosts) == 0 {
		return nil
	}
	strict := strings.EqualFold(strings.TrimSpace(viper.GetString("MN_DEPLOY_STRICT_DOMAIN_CHECKS")), "true")
	expectedCNAME := strings.TrimSpace(viper.GetString("MN_PUBLIC_DNS_EXPECT_CNAME"))

	for _, host := range hosts {
		okPrefix := fmt.Sprintf("  \033[32m✓\033[0m [Domain] %s", host)
		warnPrefix := fmt.Sprintf("  \033[33m⚠\033[0m [Domain] %s", host)

		cname, cnameErr := net.LookupCNAME(host)
		ips, ipErr := net.LookupHost(host)
		if ipErr != nil || len(ips) == 0 {
			msg := fmt.Sprintf("%s DNS lookup failed: %v", warnPrefix, ipErr)
			if strict {
				return fmt.Errorf("public domain readiness: %s", msg)
			}
			fmt.Println(msg)
			continue
		}
		if cnameErr == nil && expectedCNAME != "" && !strings.Contains(strings.ToLower(cname), strings.ToLower(expectedCNAME)) {
			msg := fmt.Sprintf("%s CNAME %q does not match expected %q", warnPrefix, cname, expectedCNAME)
			if strict {
				return fmt.Errorf("public domain readiness: %s", msg)
			}
			fmt.Println(msg)
		}

		// TLS handshake with SNI validation.
		dialer := &net.Dialer{Timeout: 8 * time.Second}
		conn, tlsErr := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, "443"), &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if tlsErr != nil {
			msg := fmt.Sprintf("%s TLS check failed: %v", warnPrefix, tlsErr)
			if strict {
				return fmt.Errorf("public domain readiness: %s", msg)
			}
			fmt.Println(msg)
			continue
		}
		_ = conn.Close()

		// HTTP reachability: accept app redirects/auth responses, flag obvious route failures.
		httpClient := &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
		resp, reqErr := httpClient.Get("https://" + host + "/")
		if reqErr != nil {
			msg := fmt.Sprintf("%s HTTPS probe failed: %v", warnPrefix, reqErr)
			if strict {
				return fmt.Errorf("public domain readiness: %s", msg)
			}
			fmt.Println(msg)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
			msg := fmt.Sprintf("%s unexpected HTTP status %d", warnPrefix, resp.StatusCode)
			if strict {
				return fmt.Errorf("public domain readiness: %s", msg)
			}
			fmt.Println(msg)
			continue
		}

		if cnameErr == nil {
			fmt.Printf("%s DNS ok (%s), HTTPS ok (status %d)\n", okPrefix, strings.TrimSuffix(cname, "."), resp.StatusCode)
		} else {
			fmt.Printf("%s DNS ok (%s), HTTPS ok (status %d)\n", okPrefix, strings.Join(ips, ","), resp.StatusCode)
		}
	}
	return nil
}
