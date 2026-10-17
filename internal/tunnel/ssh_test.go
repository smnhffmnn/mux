package tunnel

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/sshclient/sshtest"
)

// TestNewSSH_ForwardsTCP runs an SSH server and an echo listener in-process,
// dials the echo listener through the tunnel and reads its own bytes back.
func TestNewSSH_ForwardsTCP(t *testing.T) {
	signer, keyPEM := sshtest.GenerateKey(t, "pw")
	srv := sshtest.New(t, sshtest.WithAuthorizedKeys(signer.PublicKey()), sshtest.WithUser("deploy"))

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	// The tunnel verifies host keys against known_hosts files; point the
	// config dir at a temp dir holding the server's line, and HOME at an
	// empty one so neither ~/.mux nor ~/.ssh/known_hosts of the developer
	// takes part.
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got := config.Dir(); !strings.HasPrefix(got, dir) {
		t.Skipf("config.Dir() = %q does not honour XDG_CONFIG_HOME on this platform", got)
	}
	writeFile(t, config.Dir()+"/known_hosts", srv.KnownHostsLine()+"\n")

	tun, err := NewSSH(config.TunnelConfig{
		Name: "bastion", Type: config.TunnelTypeSSH,
		Host: srv.Host, Port: srv.Port, User: "deploy",
		PrivateKey: keyPEM, Passphrase: "pw",
	})
	if err != nil {
		t.Fatalf("NewSSH: %v", err)
	}
	defer tun.Close()
	if !tun.IsUp() {
		t.Fatal("tunnel not up after NewSSH")
	}

	conn, err := tun.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through tunnel = %q, %v", buf, err)
	}
}

func TestNewSSH_RefusesUnknownHost(t *testing.T) {
	_, keyPEM := sshtest.GenerateKey(t, "")
	srv := sshtest.New(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir) // no ~/.ssh/known_hosts either
	if got := config.Dir(); !strings.HasPrefix(got, dir) {
		t.Skipf("config.Dir() = %q does not honour XDG_CONFIG_HOME on this platform", got)
	}

	_, err := NewSSH(config.TunnelConfig{
		Name: "bastion", Type: config.TunnelTypeSSH,
		Host: srv.Host, Port: srv.Port, User: srv.User, PrivateKey: keyPEM,
	})
	if err == nil || !strings.Contains(err.Error(), "not known") {
		t.Fatalf("unknown host: want refusal with known_hosts hint, got %v", err)
	}

	tun, err := NewSSH(config.TunnelConfig{
		Name: "bastion", Type: config.TunnelTypeSSH,
		Host: srv.Host, Port: srv.Port, User: srv.User, PrivateKey: keyPEM,
		InsecureHostKey: true,
	})
	if err != nil {
		t.Fatalf("insecure_host_key: %v", err)
	}
	tun.Close()
}

func TestNewSSH_PassphraseRequired(t *testing.T) {
	_, keyPEM := sshtest.GenerateKey(t, "pw")
	_, err := NewSSH(config.TunnelConfig{
		Name: "bastion", Type: config.TunnelTypeSSH,
		Host: "127.0.0.1", Port: 1, User: "x", PrivateKey: keyPEM, InsecureHostKey: true,
	})
	if err == nil || !strings.Contains(err.Error(), "tunnel-bastion-private-key-passphrase") {
		t.Fatalf("want error naming the passphrase secret, got %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileImpl(path, content); err != nil {
		t.Fatal(err)
	}
}
