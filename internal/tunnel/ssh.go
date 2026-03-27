package tunnel

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/smnhffmnn/mux/internal/config"
)

// SSHTunnel provides TCP dialing through an SSH connection.
// It implements the tools.Dialer interface via DialContext.
//
// Concurrency-safe: all public methods can be called from multiple goroutines.
// Reconnection is serialized via connectMu to avoid thundering herd.
type SSHTunnel struct {
	name string
	host string
	port int
	user string

	sshCfg *ssh.ClientConfig

	mu     sync.Mutex // guards client and closed
	client *ssh.Client
	closed bool

	connectMu sync.Mutex // serializes reconnection attempts
}

// NewSSH creates and starts an SSH tunnel from the given config.
// Supports both plaintext and passphrase-protected private keys.
// If KeyFile is set and PrivateKey is empty, reads the key from the file.
func NewSSH(cfg config.TunnelConfig) (*SSHTunnel, error) {
	// Load key from file if not already in keychain
	keyData := cfg.PrivateKey
	if keyData == "" && cfg.KeyFile != "" {
		path := config.ExpandHome(cfg.KeyFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read SSH key file %s: %w", path, err)
		}
		keyData = string(data)
	}

	if keyData == "" {
		return nil, fmt.Errorf("no SSH key configured (need private key via keychain or key_file)")
	}

	// Try parsing as unencrypted key first, then with empty passphrase
	signer, err := ssh.ParsePrivateKey([]byte(keyData))
	if err != nil {
		if _, ok := err.(*ssh.PassphraseMissingError); ok {
			return nil, fmt.Errorf("SSH key is passphrase-protected — store the decrypted key in the keychain via secret_set (tunnel-%s-private-key)", cfg.Name)
		}
		return nil, fmt.Errorf("parse SSH key: %w", err)
	}

	var hostKeyCallback ssh.HostKeyCallback

	if cfg.InsecureHostKey {
		log.Printf("[ssh] Tunnel %q: host key verification disabled (insecure_host_key = true)", cfg.Name)
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		// Try loading known_hosts for host key verification
		knownHostsPath := defaultKnownHostsPath()
		if _, err := os.Stat(knownHostsPath); err == nil {
			cb, err := knownhosts.New(knownHostsPath)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w (set insecure_host_key = true to skip verification)", knownHostsPath, err)
			}
			hostKeyCallback = cb
		} else {
			return nil, fmt.Errorf("%s not found — cannot verify host key (set insecure_host_key = true to skip verification)", knownHostsPath)
		}
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	port := cfg.Port
	if port == 0 {
		port = 22
	}

	t := &SSHTunnel{
		name:   cfg.Name,
		host:   cfg.Host,
		port:   port,
		user:   cfg.User,
		sshCfg: sshCfg,
	}

	// Establish initial connection
	if err := t.connectLocked(); err != nil {
		return nil, err
	}

	// Start keepalive goroutine (C2)
	go t.keepalive()

	return t, nil
}

// Name returns the tunnel name.
func (t *SSHTunnel) Name() string { return t.name }

// DialContext dials a TCP connection through the SSH tunnel.
// Respects context cancellation. Automatically reconnects on failure.
func (t *SSHTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("SSH tunnel %q is closed", t.name)
	}
	client := t.client
	t.mu.Unlock()

	// Try dialing with context awareness
	if client != nil {
		conn, err := t.dialWithContext(ctx, client, network, address)
		if err == nil {
			return conn, nil
		}
		log.Printf("[ssh] Tunnel %q: dial failed (%v), reconnecting...", t.name, err)
	}

	// Reconnect (serialized, passes stale client for double-check)
	if err := t.reconnect(ctx, client); err != nil {
		return nil, fmt.Errorf("SSH reconnect failed: %w", err)
	}

	t.mu.Lock()
	client = t.client
	t.mu.Unlock()

	if client == nil {
		return nil, fmt.Errorf("SSH tunnel %q: no connection after reconnect", t.name)
	}

	return t.dialWithContext(ctx, client, network, address)
}

// Close shuts down the SSH connection.
func (t *SSHTunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.client != nil {
		err := t.client.Close()
		t.client = nil
		return err
	}
	return nil
}

// IsUp reports whether the SSH tunnel has an active connection.
func (t *SSHTunnel) IsUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client != nil && !t.closed
}

// dialWithContext wraps client.Dial with context cancellation support.
// On context cancel, cleans up any connection the background goroutine may have opened (S1 fix).
func (t *SSHTunnel) dialWithContext(ctx context.Context, client *ssh.Client, network, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := client.Dial(network, address)
		ch <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		// Clean up the connection if the goroutine completes after cancel
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

// reconnect serializes reconnection attempts to prevent thundering herd.
// staleClient is the client that triggered the reconnect — if another goroutine
// already replaced it, we skip the reconnect (S2 fix).
func (t *SSHTunnel) reconnect(ctx context.Context, staleClient *ssh.Client) error {
	t.connectMu.Lock()
	defer t.connectMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	// Double-check: another goroutine may have reconnected while we waited for connectMu
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("tunnel closed")
	}
	if t.client != staleClient {
		// Another goroutine already reconnected — use the new client
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	return t.connectLocked()
}

// connectLocked establishes a new SSH connection. Must be called with connectMu held or during init.
func (t *SSHTunnel) connectLocked() error {
	addr := net.JoinHostPort(t.host, fmt.Sprintf("%d", t.port))

	client, err := ssh.Dial("tcp", addr, t.sshCfg)
	if err != nil {
		return fmt.Errorf("SSH dial %s@%s: %w", t.user, addr, err)
	}

	t.mu.Lock()
	if t.closed {
		// Close() was called while we were dialing — discard the new connection (S3 fix)
		t.mu.Unlock()
		client.Close()
		return fmt.Errorf("tunnel closed during reconnect")
	}
	old := t.client
	t.client = client
	t.mu.Unlock()

	if old != nil {
		old.Close()
	}

	log.Printf("[ssh] Tunnel %q: connected to %s", t.name, addr)
	return nil
}

// keepalive sends periodic SSH requests to detect dead connections.
func (t *SSHTunnel) keepalive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		t.mu.Lock()
		closed := t.closed
		client := t.client
		t.mu.Unlock()

		if closed {
			return
		}
		if client == nil {
			continue
		}

		// Send a keepalive request; if it fails, the connection is dead
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		if err != nil {
			log.Printf("[ssh] Tunnel %q: keepalive failed (%v), marking connection dead", t.name, err)
			t.mu.Lock()
			if t.client == client {
				t.client.Close()
				t.client = nil
			}
			t.mu.Unlock()
		}
	}
}

// defaultKnownHostsPath returns ~/.ssh/known_hosts.
func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

