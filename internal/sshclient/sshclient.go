// Package sshclient is the one place in mux that opens SSH connections.
//
// Two things sit on top of it: the ssh tunnel type (TCP forwarding through a
// bastion) and the ssh connection type (commands and file copies on a host).
// Before this package existed the tunnel parsed keys, checked host keys and
// reconnected on its own; the connection type would have had to repeat all of
// it. Keeping it here means a passphrase-protected key, the user's ssh-agent
// or a pinned host key work the same way wherever mux speaks SSH — and a fix
// in one place is a fix for both.
//
// What the package does not do: decide what a host may be asked. Command
// policy belongs to the connection type that runs commands.
package sshclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// Dialer opens the TCP connection the SSH handshake runs over. A tunnel
// implements it to route one SSH hop through another; nil means the host's
// own network.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

const (
	// DefaultPort is the SSH port used when none is configured.
	DefaultPort = 22
	// DefaultTimeout bounds TCP connect plus SSH handshake. Generous on
	// purpose: a host behind a relayed overlay network (Tailscale via DERP,
	// a bastion in another region) can take ten seconds and more to the
	// first byte, and a timeout there costs a spurious failure, not safety.
	DefaultTimeout = 30 * time.Second
	// DefaultKeepaliveInterval is how often an idle connection is probed, and
	// how long a probe may take. A dead connection is noticed here instead of
	// at the next command, which would otherwise pay for the failure and the
	// reconnect.
	DefaultKeepaliveInterval = 30 * time.Second
)

// Config describes one SSH endpoint and how to authenticate against it.
type Config struct {
	Name string // for log lines; the connection or tunnel name
	Host string
	Port int // 0 = DefaultPort
	User string

	Key     KeySource
	HostKey HostKeyPolicy

	// Dialer routes the TCP leg; nil dials directly.
	Dialer Dialer
	// Timeout bounds connect and handshake; 0 = DefaultTimeout.
	Timeout time.Duration
	// KeepaliveInterval is the probe interval for idle connections;
	// 0 = DefaultKeepaliveInterval.
	KeepaliveInterval time.Duration
	// Logf receives diagnostic lines; nil = log.Printf.
	Logf func(format string, args ...any)
}

// Client is a managed SSH connection: it connects lazily (or on Connect),
// reconnects after the connection dropped, and probes an idle connection with
// keepalives. Safe for concurrent use; reconnects are serialized so a burst of
// failing calls produces one handshake, not one per call.
type Client struct {
	cfg  Config
	addr string
	auth authSource
	// authDesc describes the key source for log lines and test results.
	authDesc string
	logf     func(string, ...any)

	// hostKey is the verifier the client was created with. A pinned key is
	// final; a known_hosts policy is re-read at every handshake (see
	// handshake), so a line the user just added is honoured by the next
	// command instead of by the next restart.
	hostKey *HostKeyVerifier

	mu     sync.Mutex // guards client, closed and done
	client *ssh.Client
	closed bool
	done   chan struct{} // closed by Close; ends the keepalive loop

	connectMu sync.Mutex // serializes handshakes; never taken while mu is held
	keepalive sync.Once
	// probing is set while a keepalive probe is outstanding, so a probe that
	// waits on a slow peer is not joined by another every interval.
	probing atomic.Bool
	// lastSeen is the unix-nano time of the last byte that arrived from the
	// peer on any channel (reported by callers via Touch). The keepalive loop
	// trusts a connection that delivered data recently over a probe that is
	// slow to come back: a congested link is not a dead one.
	lastSeen atomic.Int64
}

// New validates the configuration — the private key parses (with its
// passphrase), the host key policy is usable, the agent socket is present when
// the agent is asked for — and returns a client that has not connected yet.
// Failing here, at registration, is what makes a bad key show up in the
// health report instead of at the first command hours later.
func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, errors.New("ssh: host is required")
	}
	if cfg.User == "" {
		return nil, errors.New("ssh: user is required")
	}
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.KeepaliveInterval == 0 {
		cfg.KeepaliveInterval = DefaultKeepaliveInterval
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}

	auth, authDesc, err := cfg.Key.resolve()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", cfg.Name, err)
	}
	if cfg.HostKey.Logf == nil {
		cfg.HostKey.Logf = logf
	}
	hostKey, err := cfg.HostKey.Verifier()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", cfg.Name, err)
	}

	return &Client{
		cfg:      cfg,
		addr:     net.JoinHostPort(cfg.Host, strconv.Itoa(port)),
		auth:     auth,
		authDesc: authDesc,
		logf:     logf,
		hostKey:  hostKey,
		done:     make(chan struct{}),
	}, nil
}

// Target is user@host:port, the form used in results and log lines.
func (c *Client) Target() string { return c.cfg.User + "@" + c.addr }

// Addr is host:port.
func (c *Client) Addr() string { return c.addr }

// AuthDescription says how the client authenticates ("key ed25519 SHA256:…",
// "ssh-agent"), for test results and logs.
func (c *Client) AuthDescription() string { return c.authDesc }

// HostKeyDescription says how the server is verified ("pinned SHA256:…",
// "known_hosts: /a, /b").
func (c *Client) HostKeyDescription() string { return c.hostKey.Description }

// HostKeyAlgorithms is the negotiation order offered to the server for the
// verifier as loaded at New; nil means the library default. Exposed for tests.
func (c *Client) HostKeyAlgorithms() []string {
	if c.hostKey.Algorithms == nil {
		return nil
	}
	return c.hostKey.Algorithms(c.addr)
}

// Connect establishes the connection now. Callers that want fail-fast
// semantics at startup (the tunnel) use it; the connection type lets Get
// connect on the first command instead.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.Get(ctx)
	return err
}

// Get returns the live *ssh.Client, connecting when there is none. Callers
// that then fail on the returned client pass it to Reconnect.
func (c *Client) Get(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("ssh %s: client is closed", c.cfg.Name)
	}
	client := c.client
	c.mu.Unlock()
	if client != nil {
		return client, nil
	}
	return c.Reconnect(ctx, nil)
}

// Reconnect replaces a dead connection. stale is the client the caller saw
// fail; when another caller already replaced it, the new one is returned
// without a second handshake.
func (c *Client) Reconnect(ctx context.Context, stale *ssh.Client) (*ssh.Client, error) {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("ssh %s: client is closed", c.cfg.Name)
	}
	if c.client != nil && c.client != stale {
		cur := c.client
		c.mu.Unlock()
		return cur, nil
	}
	c.mu.Unlock()

	client, err := c.handshake(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		client.Close()
		return nil, fmt.Errorf("ssh %s: closed during connect", c.cfg.Name)
	}
	old := c.client
	c.client = client
	c.mu.Unlock()
	if old != nil {
		old.Close()
	}

	c.Touch()
	c.keepalive.Do(func() { go c.keepaliveLoop() })
	c.logf("[ssh] %s: connected to %s (%s; %s)", c.cfg.Name, c.Target(), c.authDesc, c.hostKey.Description)
	return client, nil
}

// handshake dials the TCP leg and runs the SSH handshake, both bounded by
// cfg.Timeout. ssh.NewClientConn has no deadline of its own, and a connection
// forwarded through a tunnel does not support socket deadlines at all, so the
// bound is enforced from outside: the handshake runs in a goroutine, and when
// the context expires first the connection is closed under it, which makes
// every blocked read return.
func (c *Client) handshake(ctx context.Context) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	methods, cleanup, err := c.auth.methods(ctx)
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", c.cfg.Name, err)
	}
	defer cleanup()

	verifier := c.hostKey
	if c.cfg.HostKey.Pinned == "" && !c.cfg.HostKey.Insecure {
		// known_hosts may have changed since New — the unknown-host error
		// tells the user which line to add, and the next attempt must see
		// it. Fall back to the verifier from New if the files became
		// unreadable.
		if fresh, err := c.cfg.HostKey.Verifier(); err == nil {
			verifier = fresh
		} else {
			c.logf("[ssh] %s: known_hosts could not be re-read (%v), using the files as loaded at start", c.cfg.Name, err)
		}
	}
	var algos []string
	if verifier.Algorithms != nil {
		algos = verifier.Algorithms(c.addr)
	}

	dialer := c.cfg.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("ssh %s: dial %s: %w", c.cfg.Name, c.addr, err)
	}

	// A deadline on the socket where the transport supports one (a tunnel's
	// channel does not — its error is ignored), and the goroutine + close
	// below for every transport.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, c.addr, &ssh.ClientConfig{
			User:              c.cfg.User,
			Auth:              methods,
			HostKeyCallback:   verifier.Callback,
			HostKeyAlgorithms: algos,
			Timeout:           c.cfg.Timeout,
		})
		if err != nil {
			ch <- result{nil, err}
			return
		}
		_ = conn.SetDeadline(time.Time{})
		ch <- result{ssh.NewClient(sshConn, chans, reqs), nil}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			conn.Close()
			return nil, fmt.Errorf("ssh %s: %s: %w", c.cfg.Name, c.Target(), r.err)
		}
		return r.client, nil
	case <-ctx.Done():
		conn.Close()
		if r := <-ch; r.client != nil {
			r.client.Close()
		}
		return nil, fmt.Errorf("ssh %s: %s: handshake did not complete within %s (%w)", c.cfg.Name, c.Target(), c.cfg.Timeout, ctx.Err())
	}
}

// DialContext opens a TCP connection on the remote side — the tunnel use.
// A dial that fails because the SSH connection is gone triggers one
// reconnect; a dial the server refused (the forwarded target is down) is
// returned as is, because the connection is fine and reconnecting would cut
// every other forwarded connection on it.
func (c *Client) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := dialThrough(ctx, client, network, address)
	if err == nil {
		return &touchConn{Conn: conn, client: c}, nil
	}
	if !IsTransportError(err) {
		return nil, err
	}
	c.logf("[ssh] %s: dial %s failed (%v), reconnecting", c.cfg.Name, address, err)
	client, err = c.Reconnect(ctx, client)
	if err != nil {
		return nil, err
	}
	conn, err = dialThrough(ctx, client, network, address)
	if err != nil {
		return nil, err
	}
	return &touchConn{Conn: conn, client: c}, nil
}

// IsTransportError reports whether err means the SSH connection itself is
// gone, as opposed to the server refusing a request on a live connection.
// Callers reconnect only on the former: an *ssh.OpenChannelError for a
// refused channel, a failed subsystem request or an exit status all come from
// a peer that is still there, and tearing the connection down would end every
// other session running on it.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	var oce *ssh.OpenChannelError
	if errors.As(err, &oce) {
		return false
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"use of closed network connection", "connection reset", "broken pipe",
		"ssh: unexpected packet", "ssh: disconnect", "client is closed", "closed during connect",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// dialThrough wraps client.Dial, which has no context, so a cancelled caller
// does not hang on a remote that never answers. A connection that completes
// after the cancel is closed rather than leaked.
func dialThrough(ctx context.Context, client *ssh.Client, network, address string) (net.Conn, error) {
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

// touchConn reports incoming bytes on a forwarded connection to the client,
// so a tunnel carrying traffic counts as alive.
type touchConn struct {
	net.Conn
	client *Client
}

func (t *touchConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.client.Touch()
	}
	return n, err
}

// Touch records that data arrived from the peer just now. Callers that read
// from a channel (command output, file transfers, forwarded TCP) call it so
// the keepalive loop does not mistake a slow transfer for a dead connection —
// the same rule OpenSSH applies to ServerAliveInterval, which only counts an
// interval without any received packet.
func (c *Client) Touch() { c.lastSeen.Store(time.Now().UnixNano()) }

// sinceLastSeen is the time since Touch was last called (or a long time when
// it never was).
func (c *Client) sinceLastSeen() time.Duration {
	ns := c.lastSeen.Load()
	if ns == 0 {
		return time.Hour
	}
	return time.Since(time.Unix(0, ns))
}

// Invalidate drops client if it is still the current connection, so the next
// Get reconnects. Callers use it after an operation failed in a way that
// points at the connection rather than at the command.
func (c *Client) Invalidate(client *ssh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client != nil && c.client == client {
		c.client.Close()
		c.client = nil
	}
}

// IsCurrent reports whether client is still the connection in use.
func (c *Client) IsCurrent(client *ssh.Client) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return client != nil && c.client == client
}

// IsUp reports whether a connection is currently established. After a failed
// keepalive it is false until the next call reconnects.
func (c *Client) IsUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client != nil && !c.closed
}

// Close shuts the connection down for good and stops the keepalive loop; Get
// fails afterwards.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// keepaliveLoop probes an idle connection; a probe that fails while nothing
// else arrived either marks the connection dead, so the next call reconnects
// instead of failing. A connection that delivered data within the interval is
// not probed at all, and a probe that times out while data kept arriving is
// ignored — slow is not dead. The loop ends when the client is closed.
func (c *Client) keepaliveLoop() {
	interval := c.cfg.KeepaliveInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		client := c.client
		c.mu.Unlock()
		if client == nil || c.sinceLastSeen() < interval {
			continue
		}
		if !c.probing.CompareAndSwap(false, true) {
			continue // the previous probe is still waiting; do not stack another
		}
		go func(client *ssh.Client) {
			defer c.probing.Store(false)
			err := keepaliveProbe(client, interval)
			if err == nil {
				c.Touch()
				return
			}
			if c.sinceLastSeen() < interval {
				return // data arrived while the probe was pending
			}
			c.logf("[ssh] %s: keepalive failed (%v) with no traffic for %s, marking connection dead", c.cfg.Name, err, c.sinceLastSeen().Round(time.Second))
			c.Invalidate(client)
		}(client)
	}
}

// keepaliveProbe sends one keepalive and waits at most timeout for the reply.
// SendRequest itself blocks until the peer answers or TCP gives up, which on
// a connection that silently died (a relayed overlay path dropping out) is
// minutes — long enough for every command in between to pile up behind it.
// A probe that outlives the timeout stays parked inside the library until the
// connection closes; the caller stops issuing new ones meanwhile.
func keepaliveProbe(client *ssh.Client, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("no reply within %s", timeout)
	}
}
