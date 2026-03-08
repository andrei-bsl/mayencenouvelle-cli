// Package database — SSH tunnel support for reaching PostgreSQL from outside
// the lab network. The tunnel dials a jump host via SSH and sets up a local
// port-forward to the PG server so the provisioner can connect through it.
//
// Usage:
//
//	t, err := OpenTunnel(ctx, cfg, "db.mayencenouvelle.internal", 5432)
//	if err != nil { ... }
//	defer t.Close()
//	// then pass t.LocalAddr() as the host:port in the provisioner config
package database

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// TunnelConfig is the SSH jump host configuration stored in base.yaml under
// database.ssh_tunnel. When Enabled is true, mn-cli opens a transparent local
// port-forward to the PG server before calling EnsureDatabase.
type TunnelConfig struct {
	// Enabled gates the tunnel. If false the provisioner connects directly.
	Enabled bool `yaml:"enabled"`
	// Host is the SSH jump host (e.g. "lab-nas01.mayencenouvelle.internal").
	Host string `yaml:"host"`
	// Port is the SSH port on the jump host (default: 22).
	Port int `yaml:"port"`
	// User is the SSH login username (default: current OS user).
	User string `yaml:"user"`
	// KeyPath is the path to the SSH private key file.
	// Supports ~ expansion (default: ~/.ssh/id_ed25519, then ~/.ssh/id_rsa).
	KeyPath string `yaml:"key_path"`
}

// Tunnel is an active SSH port-forward. Connections to LocalAddr() are
// transparently forwarded through the jump host to the remote PG server.
// Call Close() when the provisioner is done to clean up the SSH session.
type Tunnel struct {
	sshClient  *ssh.Client
	listener   net.Listener
	remoteHost string
	remotePort int
}

// LocalAddr returns the local address (host:port) to use instead of the real
// PG address when connecting through the tunnel.
func (t *Tunnel) LocalAddr() (string, int) {
	addr := t.listener.Addr().(*net.TCPAddr)
	return "localhost", addr.Port
}

// Close shuts down the local listener and SSH session.
func (t *Tunnel) Close() {
	if t.listener != nil {
		t.listener.Close()
	}
	if t.sshClient != nil {
		t.sshClient.Close()
	}
}

// OpenTunnel opens an SSH connection to cfg.Host and starts a local port-forward
// to remoteHost:remotePort. The caller must call Tunnel.Close() when done.
//
// Note: ctx is checked for prior cancellation before dialing, but the underlying
// ssh.Dial and net.Listen calls are not context-aware. Callers should not rely on
// context deadlines or cancellation interrupting the connection once it has started.
func OpenTunnel(ctx context.Context, cfg TunnelConfig, remoteHost string, remotePort int) (*Tunnel, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ssh tunnel: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("ssh tunnel: host is required")
	}
	jumpPort := cfg.Port
	if jumpPort == 0 {
		jumpPort = 22
	}

	// ── Resolve SSH private key ──────────────────────────────────────────
	keyPath, err := resolveSSHKeyPath(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: %w", err)
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: read key %s: %w", keyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: parse private key %s: %w", keyPath, err)
	}

	// ── Connect to jump host ────────────────────────────────────────────
	user := cfg.User
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = "root"
	}

	sshCfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// We're connecting to a known internal lab host; skip strict host-key
		// checking rather than requiring ~/.ssh/known_hosts to be pre-populated.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
	}

	jumpAddr := fmt.Sprintf("%s:%d", cfg.Host, jumpPort)
	sshClient, err := ssh.Dial("tcp", jumpAddr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: dial %s: %w", jumpAddr, err)
	}

	// ── Local listener on a random available port ─────────────────────────
	local, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("ssh tunnel: local listener: %w", err)
	}

	t := &Tunnel{
		sshClient:  sshClient,
		listener:   local,
		remoteHost: remoteHost,
		remotePort: remotePort,
	}

	// ── Accept + forward goroutine ────────────────────────────────────────
	go t.acceptLoop()

	return t, nil
}

// acceptLoop accepts local connections and forwards each through the SSH tunnel.
func (t *Tunnel) acceptLoop() {
	remoteAddr := fmt.Sprintf("%s:%d", t.remoteHost, t.remotePort)
	for {
		localConn, err := t.listener.Accept()
		if err != nil {
			// listener closed — normal shutdown
			return
		}
		go func(lc net.Conn) {
			defer lc.Close()
			remoteConn, err := t.sshClient.Dial("tcp", remoteAddr)
			if err != nil {
				return
			}
			defer remoteConn.Close()
			biCopy(lc, remoteConn)
		}(localConn)
	}
}

// biCopy copies data in both directions between two net.Conn, blocking until
// both directions are complete.
func biCopy(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }() //nolint:errcheck
	go func() { io.Copy(b, a); done <- struct{}{} }() //nolint:errcheck
	<-done
	<-done
}

// resolveSSHKeyPath expands ~ and falls back to known default key locations.
func resolveSSHKeyPath(path string) (string, error) {
	if path != "" {
		return expandHome(path), nil
	}
	// Auto-detect common key files under ~/.ssh/
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no SSH private key found — set database.ssh_tunnel.key_path in base.yaml")
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
