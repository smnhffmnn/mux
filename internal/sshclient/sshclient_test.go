package sshclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/smnhffmnn/mux/internal/sshclient/sshtest"
)

func discardLog(string, ...any) {}

func TestKeySource_ParsePlainAndEncrypted(t *testing.T) {
	_, plain := sshtest.GenerateKey(t, "")
	_, encrypted := sshtest.GenerateKey(t, "hunter2")

	if _, _, err := (KeySource{PrivateKey: plain}).resolve(); err != nil {
		t.Fatalf("plain key: %v", err)
	}

	_, _, err := (KeySource{PrivateKey: encrypted, SecretName: "tunnel-x-private-key"}).resolve()
	if err == nil || !strings.Contains(err.Error(), "tunnel-x-private-key-passphrase") {
		t.Fatalf("encrypted key without passphrase: want error naming the passphrase secret, got %v", err)
	}

	_, _, err = (KeySource{PrivateKey: encrypted, Passphrase: "wrong", SecretName: "tunnel-x-private-key"}).resolve()
	if err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong passphrase: want decrypt error, got %v", err)
	}

	_, desc, err := (KeySource{PrivateKey: encrypted, Passphrase: "hunter2"}).resolve()
	if err != nil {
		t.Fatalf("right passphrase: %v", err)
	}
	if !strings.HasPrefix(desc, "key ssh-ed25519 SHA256:") {
		t.Errorf("description = %q, want key type and fingerprint", desc)
	}

	// A passphrase on an unencrypted key is ignored — a stale secret must not
	// break a key that was re-generated without one.
	if _, _, err := (KeySource{PrivateKey: plain, Passphrase: "stale"}).resolve(); err != nil {
		t.Fatalf("plain key with stale passphrase: %v", err)
	}
}

func TestKeySource_KeyFileAndMissing(t *testing.T) {
	_, plain := sshtest.GenerateKey(t, "")
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (KeySource{KeyFile: path}).resolve(); err != nil {
		t.Fatalf("key_file: %v", err)
	}

	_, _, err := (KeySource{SecretName: "deploy-private-key"}).resolve()
	if err == nil || !strings.Contains(err.Error(), "deploy-private-key") || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("no key: want error naming the secret and the agent alternative, got %v", err)
	}

	_, _, err = (KeySource{Auth: "password"}).resolve()
	if err == nil || !strings.Contains(err.Error(), `"key" or "agent"`) {
		t.Fatalf("bad auth: got %v", err)
	}
}

func TestKeySource_AgentWithoutSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, _, err := (KeySource{Auth: AuthAgent}).resolve()
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("agent without socket: got %v", err)
	}
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "nope"))
	_, _, err = (KeySource{Auth: AuthAgent}).resolve()
	if err == nil || !strings.Contains(err.Error(), "not there") {
		t.Fatalf("agent with dead socket path: got %v", err)
	}
}

func TestParsePublicKey(t *testing.T) {
	srv := sshtest.New(t)
	for _, line := range []string{srv.AuthorizedKeyLine(), srv.KnownHostsLine(), "  " + srv.AuthorizedKeyLine() + "  "} {
		key, err := ParsePublicKey(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if string(key.Marshal()) != string(srv.HostKey.Marshal()) {
			t.Errorf("%q parsed to a different key", line)
		}
	}
	for _, bad := range []string{"", "ssh-ed25519", "not a key at all", "ssh-ed25519 AAAA\nssh-ed25519 BBBB"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestClient_PinnedHostKey(t *testing.T) {
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")

	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key:     KeySource{PrivateKey: keyPEM},
		HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		Logf:    discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect with pinned key: %v", err)
	}
	if !c.IsUp() {
		t.Error("IsUp = false after Connect")
	}
	if !strings.HasPrefix(c.HostKeyDescription(), "pinned ssh-ed25519 SHA256:") {
		t.Errorf("host key description = %q", c.HostKeyDescription())
	}

	// A different pinned key must be refused as a mismatch.
	other := sshtest.New(t)
	c2, err := New(Config{
		Name: "t2", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key:     KeySource{PrivateKey: keyPEM},
		HostKey: HostKeyPolicy{Pinned: other.AuthorizedKeyLine()},
		Logf:    discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	err = c2.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Fatalf("mismatched pin: want HOST KEY MISMATCH, got %v", err)
	}
}

func TestClient_KnownHosts(t *testing.T) {
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")
	dir := t.TempDir()
	missing := filepath.Join(dir, "known_hosts")

	// No file at all: unknown host, error carries the line to paste.
	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{KnownHosts: []string{missing}},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Connect(context.Background())
	c.Close()
	if err == nil || !strings.Contains(err.Error(), "not known") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("unknown host: got %v", err)
	}
	// The message ends with the known_hosts line; it must parse and match.
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	pasted := lines[len(lines)-1]
	// With nothing known the client offers the library default order, so the
	// server may present either of its keys; the line must name one of them.
	_, _, key, _, _, perr := ssh.ParseKnownHosts([]byte(pasted))
	if perr != nil || !srv.HasHostKey(key) {
		t.Fatalf("pasted line %q does not parse to a server key: %v", pasted, perr)
	}

	// With the line in the file the connection succeeds.
	if err := os.WriteFile(missing, []byte(pasted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{KnownHosts: []string{missing}},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect with known_hosts: %v", err)
	}
	c.Close()

	// A different key for the same host is a mismatch, not an unknown host.
	other := sshtest.New(t)
	wrong := strings.Replace(other.KnownHostsLine(), "["+other.Host+"]:"+itoa(other.Port), "["+srv.Host+"]:"+itoa(srv.Port), 1)
	if err := os.WriteFile(missing, []byte(wrong+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{KnownHosts: []string{missing}},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Connect(context.Background())
	c.Close()
	if err == nil || !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Fatalf("changed host key: want HOST KEY MISMATCH, got %v", err)
	}
}

func TestClient_PublicKeyAuthAndReconnect(t *testing.T) {
	signer, keyPEM := sshtest.GenerateKey(t, "pw")
	srv := sshtest.New(t, sshtest.WithAuthorizedKeys(signer.PublicKey()), sshtest.WithUser("deploy"))

	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: "deploy",
		Key:     KeySource{PrivateKey: keyPEM, Passphrase: "pw"},
		HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		Logf:    discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	first, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	again, _ := c.Get(ctx)
	if again != first {
		t.Error("second Get returned a new client although the first is alive")
	}

	// The server drops us; the stale client must be replaced on Reconnect.
	srv.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	var probeErr error
	for time.Now().Before(deadline) {
		if _, _, probeErr = first.SendRequest("keepalive@openssh.com", true, nil); probeErr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if probeErr == nil {
		t.Fatal("stale client still answers after the server dropped the connection")
	}
	fresh, err := c.Reconnect(ctx, first)
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if fresh == first {
		t.Error("Reconnect returned the stale client")
	}
	if _, _, err := fresh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("fresh client is not usable: %v", err)
	}

	// Wrong user is refused by the server's auth callback.
	bad, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: "nobody",
		Key:     KeySource{PrivateKey: keyPEM, Passphrase: "pw"},
		HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		Logf:    discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.Connect(ctx); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("wrong user: want auth failure, got %v", err)
	}
}

func TestClient_Closed(t *testing.T) {
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")
	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := c.Get(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Get after Close: got %v", err)
	}
	if c.IsUp() {
		t.Error("IsUp after Close")
	}
}

func TestClient_DialTimeout(t *testing.T) {
	// 192.0.2.1 is TEST-NET-1: never routable, so the dial has to time out.
	_, keyPEM := sshtest.GenerateKey(t, "")
	c, err := New(Config{
		Name: "t", Host: "192.0.2.1", Port: 22, User: "x",
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{Insecure: true},
		Timeout: 200 * time.Millisecond, Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("connect to TEST-NET-1 succeeded?")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("dial took %s, timeout not applied", time.Since(start))
	}
}

func TestDefaultKnownHostsFiles(t *testing.T) {
	files := DefaultKnownHostsFiles("/cfg")
	if len(files) < 1 || files[0] != filepath.Join("/cfg", "known_hosts") {
		t.Fatalf("files = %v, want config dir first", files)
	}
	if len(files) == 2 && !strings.HasSuffix(files[1], filepath.Join(".ssh", "known_hosts")) {
		t.Errorf("second file = %q, want ~/.ssh/known_hosts", files[1])
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestClient_KnownHostsSkipsJunkLines: OpenSSH ignores a malformed known_hosts
// line; mux must not let one take every SSH connection down.
func TestClient_KnownHostsSkipsJunkLines(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // the leftover check below must see only this test's copies
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")
	file := filepath.Join(t.TempDir(), "known_hosts")
	content := "# comment\n" +
		"some.host.example: Connection timed out\n" + // an ssh error message a script appended
		strings.TrimSpace(srv.KnownHostsLine()) + " added by ops 2026-10\n" + // multi-word comment: valid for the parser, must survive
		"broken line without a key\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key:     KeySource{PrivateKey: keyPEM},
		HostKey: HostKeyPolicy{KnownHosts: []string{file}},
		Logf:    func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		t.Fatalf("New with junk lines: %v", err)
	}
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// The files are read at New and again at the handshake, so each junk line
	// is reported per read; count distinct lines.
	skipped := map[string]bool{}
	for _, l := range logged {
		if i := strings.Index(l, "skipping line"); i >= 0 {
			skipped[l[:i]] = true
		}
	}
	if len(skipped) != 2 {
		t.Errorf("skipped %d distinct lines, want 2 (lines 2 and 4): %v", len(skipped), logged)
	}
	// The temp copy must be gone.
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "mux-known_hosts-*"))
	if len(matches) != 0 {
		t.Errorf("temporary known_hosts copies left behind: %v", matches)
	}
}

// TestHostKeyAlgorithms: a host known by its Ed25519 key must negotiate that
// key although the library's default order lists Ed25519 last and the server
// also holds an ECDSA key. Pinned keys get the same treatment.
func TestHostKeyAlgorithms(t *testing.T) {
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")

	// Library default: a key type the server has but known_hosts does not.
	if algos := ssh.SupportedAlgorithms().HostKeys; algos[len(algos)-1] != ssh.KeyAlgoED25519 {
		t.Skipf("library default order changed (%v); the scenario no longer applies", algos)
	}

	file := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(file, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{KnownHosts: []string{file}},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if algos := c.HostKeyAlgorithms(); len(algos) == 0 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v, want ssh-ed25519 first", algos)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect to a host known by its ed25519 key: %v", err)
	}

	// Unknown host: nothing known, library default (nil).
	c2, err := New(Config{
		Name: "t", Host: "203.0.113.5", Port: 22, User: "x",
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{KnownHosts: []string{file}},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if algos := c2.HostKeyAlgorithms(); algos != nil {
		t.Errorf("unknown host: algorithms = %v, want nil", algos)
	}

	// RSA known key expands to the rsa-sha2 algorithms, ahead of the defaults.
	got := preferAlgorithms([]string{ssh.KeyAlgoRSA})
	if got[0] != ssh.KeyAlgoRSASHA512 || got[1] != ssh.KeyAlgoRSASHA256 || got[2] != ssh.KeyAlgoRSA {
		t.Errorf("rsa preference = %v", got[:3])
	}
	if len(got) != len(ssh.SupportedAlgorithms().HostKeys)+1 {
		// ssh-rsa itself is not in the defaults; everything else must be
		// deduplicated.
		t.Errorf("preferAlgorithms produced duplicates or dropped entries: %v", got)
	}
}

// TestClient_KeepaliveTrustsTraffic: a server that never answers keepalives
// but keeps delivering channel data is slow, not dead. Once the data stops,
// the unanswered probe marks the connection dead.
func TestClient_KeepaliveTrustsTraffic(t *testing.T) {
	const keepaliveInterval = 150 * time.Millisecond
	srv := sshtest.New(t, sshtest.WithoutKeepaliveReplies())
	_, keyPEM := sshtest.GenerateKey(t, "")

	// An echo listener the tunnel path forwards to; its bytes count as traffic.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(conn, conn); conn.Close() }()
		}
	}()

	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		KeepaliveInterval: keepaliveInterval, Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := c.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("dial through: %v", err)
	}
	defer conn.Close()

	// Keep traffic flowing for several keepalive intervals.
	deadline := time.Now().Add(6 * keepaliveInterval)
	buf := make([]byte, 4)
	for time.Now().Before(deadline) {
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write during traffic: %v", err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read during traffic: %v", err)
		}
		time.Sleep(keepaliveInterval / 3)
	}
	if !c.IsUp() {
		t.Fatal("connection marked dead although data kept arriving")
	}

	// Silence: the next unanswered probe must mark it dead.
	deadline = time.Now().Add(10 * keepaliveInterval)
	for c.IsUp() && time.Now().Before(deadline) {
		time.Sleep(keepaliveInterval / 3)
	}
	if c.IsUp() {
		t.Fatal("idle connection with unanswered keepalives was never marked dead")
	}
}

// TestClient_AgentAuth signs through an in-process ssh-agent: the key never
// enters the client, and the agent socket is closed once the handshake is over.
func TestClient_AgentAuth(t *testing.T) {
	keyring := agent.NewKeyring()
	authSigner, authPEM := sshtest.GenerateKey(t, "")
	srv2 := sshtest.New(t, sshtest.WithAuthorizedKeys(authSigner.PublicKey()), sshtest.WithUser("deploy"))
	rawKey, err := ssh.ParseRawPrivateKey([]byte(authPEM))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: rawKey, Comment: "test"}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var served atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			go func() { agent.ServeAgent(keyring, conn); conn.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)

	c, err := New(Config{
		Name: "t", Host: srv2.Host, Port: srv2.Port, User: "deploy",
		Key:     KeySource{Auth: AuthAgent},
		HostKey: HostKeyPolicy{Pinned: srv2.AuthorizedKeyLine()},
		Logf:    discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.AuthDescription() != "ssh-agent" {
		t.Errorf("auth description = %q", c.AuthDescription())
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect via agent: %v", err)
	}
	if served.Load() != 1 {
		t.Errorf("agent served %d connections, want 1 per handshake", served.Load())
	}
}

// TestClient_HandshakeTimeoutThroughDialer: a transport that accepts the TCP
// connection but never speaks SSH, and that rejects socket deadlines the way
// a tunnel channel does, must still be bounded by the handshake timeout.
func TestClient_HandshakeTimeoutThroughDialer(t *testing.T) {
	_, keyPEM := sshtest.GenerateKey(t, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open and say nothing.
			go func() { io.Copy(io.Discard, conn) }()
		}
	}()

	c, err := New(Config{
		Name: "t", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, User: "x",
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{Insecure: true},
		Dialer:  noDeadlineDialer{},
		Timeout: 300 * time.Millisecond, Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	err = c.Connect(context.Background())
	if err == nil {
		t.Fatal("handshake against a silent peer succeeded?")
	}
	if !strings.Contains(err.Error(), "did not complete within") {
		t.Errorf("error = %v, want the handshake timeout", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("handshake took %s, not bounded", time.Since(start))
	}
}

// noDeadlineDialer dials directly but hands out connections that refuse
// deadlines, like the channel a tunnel returns.
type noDeadlineDialer struct{}

func (noDeadlineDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return noDeadlineConn{conn}, nil
}

type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetDeadline(time.Time) error      { return errors.New("deadline not supported") }
func (noDeadlineConn) SetReadDeadline(time.Time) error  { return errors.New("deadline not supported") }
func (noDeadlineConn) SetWriteDeadline(time.Time) error { return errors.New("deadline not supported") }

// TestClient_DialRefusedKeepsConnection: a forwarded dial the server refuses
// (target port closed) is an error for that dial only — the SSH connection
// stays, so other forwarded connections on it are not cut.
func TestClient_DialRefusedKeepsConnection(t *testing.T) {
	srv := sshtest.New(t)
	_, keyPEM := sshtest.GenerateKey(t, "")
	c, err := New(Config{
		Name: "t", Host: srv.Host, Port: srv.Port, User: srv.User,
		Key: KeySource{PrivateKey: keyPEM}, HostKey: HostKeyPolicy{Pinned: srv.AuthorizedKeyLine()},
		Logf: discardLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	first, err := c.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A port nothing listens on.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()
	if _, err := c.DialContext(ctx, "tcp", addr); err == nil {
		t.Fatal("dial to a closed port succeeded?")
	}
	if cur, _ := c.Get(ctx); cur != first {
		t.Error("a refused forwarded dial replaced the SSH connection")
	}

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed"}, false},
		{&ssh.ExitError{}, false},
		{io.EOF, true},
		{net.ErrClosed, true},
		{errors.New("ssh: unexpected packet in response to channel open"), true},
		{errors.New("ssh: subsystem request failed"), false},
		{nil, false},
	} {
		if got := IsTransportError(tc.err); got != tc.want {
			t.Errorf("IsTransportError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
