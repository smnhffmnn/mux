package tunnel

import (
	"context"
	"fmt"
	"net"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/sshclient"
)

// SSHTunnel provides TCP dialing through an SSH connection. It implements the
// tools.Dialer interface via DialContext.
//
// The connection itself — key sources, host key policy, reconnect, keepalive —
// lives in sshclient and is shared with the ssh connection type; this type only
// adapts a TunnelConfig to it and keeps the fail-fast start that tunnels need
// (a tunnel that cannot connect at startup takes its connections with it).
type SSHTunnel struct {
	name   string
	client *sshclient.Client
}

// NewSSH creates and starts an SSH tunnel from the given config.
//
// The private key comes from the secret store (tunnel-{name}-private-key, with
// tunnel-{name}-private-key-passphrase when it is encrypted) or from key_file;
// auth = "agent" uses the user's ssh-agent instead. Host keys are checked
// against known_hosts in the mux config directory and ~/.ssh/known_hosts
// unless insecure_host_key is set.
func NewSSH(cfg config.TunnelConfig) (*SSHTunnel, error) {
	client, err := sshclient.New(sshclient.Config{
		Name: cfg.Name,
		Host: cfg.Host,
		Port: cfg.Port,
		User: cfg.User,
		Key: sshclient.KeySource{
			Auth:       cfg.Auth,
			PrivateKey: cfg.PrivateKey,
			Passphrase: cfg.Passphrase,
			KeyFile:    config.ExpandHome(cfg.KeyFile),
			SecretName: "tunnel-" + cfg.Name + "-private-key",
		},
		HostKey: sshclient.HostKeyPolicy{
			KnownHosts: sshclient.DefaultKnownHostsFiles(config.Dir()),
			Insecure:   cfg.InsecureHostKey,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := client.Connect(context.Background()); err != nil {
		client.Close()
		return nil, err
	}
	return &SSHTunnel{name: cfg.Name, client: client}, nil
}

// Name returns the tunnel name.
func (t *SSHTunnel) Name() string { return t.name }

// DialContext dials a TCP connection through the SSH tunnel. Respects context
// cancellation and reconnects once when the SSH connection has died.
func (t *SSHTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := t.client.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("SSH tunnel %q: %w", t.name, err)
	}
	return conn, nil
}

// Close shuts down the SSH connection.
func (t *SSHTunnel) Close() error { return t.client.Close() }

// IsUp reports whether the SSH tunnel has an active connection.
func (t *SSHTunnel) IsUp() bool { return t.client.IsUp() }
