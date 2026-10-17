// Package sshtest runs an SSH server inside a test process, so sshclient and
// the ssh connection type can be tested end to end — handshake, host key
// verification, exec with exit status and signals, the SFTP subsystem —
// without an sshd on the machine or in CI.
//
// Commands run through /bin/sh on the test machine. Tests therefore stick to
// portable commands (echo, cat, sleep, exit).
package sshtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"time"
)

// Server is a running in-process SSH server.
type Server struct {
	// Addr is host:port of the listener.
	Addr string
	// Host and Port split Addr for configs that take them apart.
	Host string
	Port int
	// HostKey is the server's Ed25519 public key, for pinning or known_hosts
	// lines. HostKeys lists every key the server holds (ECDSA and Ed25519):
	// which one a client is shown depends on the algorithms it asks for.
	HostKey  ssh.PublicKey
	HostKeys []ssh.PublicKey
	// User is the only login the server accepts.
	User string

	listener     net.Listener
	config       *ssh.ServerConfig
	ignoreGlobal bool
	ignoreSignal bool
	sftpDelay    time.Duration
	wg           sync.WaitGroup

	mu       sync.Mutex
	conns    []net.Conn
	commands []string
	running  []*exec.Cmd
	closed   bool
}

// Option configures New.
type Option func(*options)

type options struct {
	authorized   []ssh.PublicKey
	user         string
	ignoreGlobal bool
	ignoreSignal bool
	sftpDelay    time.Duration
}

// WithAuthorizedKeys restricts logins to these public keys. Without it the
// server accepts any client (NoClientAuth) — fine for tests of everything
// except authentication.
func WithAuthorizedKeys(keys ...ssh.PublicKey) Option {
	return func(o *options) { o.authorized = append(o.authorized, keys...) }
}

// WithUser sets the accepted login (default "tester").
func WithUser(user string) Option {
	return func(o *options) { o.user = user }
}

// WithoutKeepaliveReplies makes the server swallow global requests, so a
// client's keepalive probe never gets an answer — the shape of a link that
// still moves channel data but stalls on everything else.
func WithoutKeepaliveReplies() Option {
	return func(o *options) { o.ignoreGlobal = true }
}

// WithoutSignalSupport makes the server ignore "signal" channel requests, as
// sshd before 7.9 and sessions under a forced command do. A client's SIGKILL
// then does nothing; only closing the channel ends the session.
func WithoutSignalSupport() Option {
	return func(o *options) { o.ignoreSignal = true }
}

// WithSlowSFTP delays every read and write of the SFTP subsystem's channel by
// d — a slow link for transfer-timeout tests.
func WithSlowSFTP(d time.Duration) Option {
	return func(o *options) { o.sftpDelay = d }
}

// New starts a server on 127.0.0.1 and stops it when the test ends.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sshtest runs commands through /bin/sh, which Windows has not")
	}
	o := options{user: "tester"}
	for _, opt := range opts {
		opt(&o)
	}

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshtest: generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("sshtest: host signer: %v", err)
	}

	cfg := &ssh.ServerConfig{}
	if len(o.authorized) == 0 {
		cfg.NoClientAuth = true
	} else {
		cfg.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() != o.user {
				return nil, fmt.Errorf("unknown user %q", conn.User())
			}
			for _, k := range o.authorized {
				if string(k.Marshal()) == string(key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("key not authorized")
		}
	}
	// A real sshd holds RSA, ECDSA and Ed25519 keys and presents whichever the
	// client lists first. Offering all three makes the tests honest about
	// algorithm negotiation: a client that does not ask for Ed25519 is shown
	// another key, and must still verify against the Ed25519 line it knows.
	ecdsaPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("sshtest: generate ecdsa host key: %v", err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaPriv)
	if err != nil {
		t.Fatalf("sshtest: ecdsa signer: %v", err)
	}
	cfg.AddHostKey(ecdsaSigner)
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	s := &Server{
		ignoreGlobal: o.ignoreGlobal,
		ignoreSignal: o.ignoreSignal,
		sftpDelay:    o.sftpDelay,
		Addr:         ln.Addr().String(),
		Host:         addr.IP.String(),
		Port:         addr.Port,
		HostKey:      hostSigner.PublicKey(),
		HostKeys:     []ssh.PublicKey{ecdsaSigner.PublicKey(), hostSigner.PublicKey()},
		User:         o.user,
		listener:     ln,
		config:       cfg,
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// Commands returns every exec command string received so far.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// DropConnections closes every client connection without stopping the
// server, to test reconnection.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Close stops the listener, kills commands still running and drops all
// connections.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	running := s.running
	s.running = nil
	s.mu.Unlock()
	s.listener.Close()
	for _, cmd := range running {
		if cmd.Process != nil {
			signalProcessGroup(cmd, true)
		}
	}
	s.DropConnections()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer s.wg.Done()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		conn.Close()
		return
	}
	defer sconn.Close()
	go func() {
		for req := range reqs {
			// keepalive@openssh.com and friends: answer so the client's
			// keepalive sees a live connection — unless the test wants a
			// server that never does.
			if req.WantReply && !s.ignoreGlobal {
				req.Reply(true, nil)
			}
		}
	}()
	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			s.wg.Add(1)
			go s.session(ch, chReqs)
		case "direct-tcpip":
			// Port forwarding, what the tunnel type uses.
			var payload struct {
				DestAddr string
				DestPort uint32
				OrigAddr string
				OrigPort uint32
			}
			if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
				newCh.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
				continue
			}
			target, err := net.Dial("tcp", net.JoinHostPort(payload.DestAddr, fmt.Sprint(payload.DestPort)))
			if err != nil {
				newCh.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				target.Close()
				continue
			}
			go ssh.DiscardRequests(chReqs)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer ch.Close()
				defer target.Close()
				done := make(chan struct{}, 2)
				go func() { io.Copy(ch, target); done <- struct{}{} }()
				go func() { io.Copy(target, ch); done <- struct{}{} }()
				<-done
			}()
		default:
			newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

// session handles one channel: exec runs the command through /bin/sh and
// reports exit-status; the sftp subsystem serves the test machine's file
// system; everything else is refused.
func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()
	defer ch.Close()

	var cmd *exec.Cmd
	var done chan struct{}
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil || cmd != nil {
				req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.commands = append(s.commands, payload.Command)
			s.mu.Unlock()

			cmd = exec.Command("/bin/sh", "-c", payload.Command)
			cmd.Stdin = ch
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			// Own process group, so a signal reaches children too (sh -c
			// "sleep 30" leaves an orphan otherwise), and a bounded wait for
			// the output pipes once the leader has exited.
			setProcessGroup(cmd)
			cmd.WaitDelay = time.Second
			if err := cmd.Start(); err != nil {
				req.Reply(false, nil)
				return
			}
			s.mu.Lock()
			s.running = append(s.running, cmd)
			s.mu.Unlock()
			req.Reply(true, nil)
			done = make(chan struct{})
			go func(c *exec.Cmd) {
				defer close(done)
				// Like sshd: report the exit, then close the channel. The
				// client's Session.Wait returns only once the channel is
				// closed, not on EOF — a server that merely half-closes
				// leaves every client hanging.
				defer ch.Close()
				status := uint32(0)
				if err := c.Wait(); err != nil {
					if ee, ok := err.(*exec.ExitError); ok {
						if sig, ok := exitSignal(ee); ok {
							ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
								Signal     string
								CoreDumped bool
								Error      string
								Lang       string
							}{sig, false, "", ""}))
							return
						}
						status = uint32(ee.ExitCode())
					} else {
						status = 255
					}
				}
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			}(cmd)
		case "subsystem":
			var payload struct{ Name string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil || payload.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			var rwc io.ReadWriteCloser = ch
			if s.sftpDelay > 0 {
				rwc = &slowRWC{ReadWriteCloser: ch, delay: s.sftpDelay}
			}
			srv, err := sftp.NewServer(rwc)
			if err != nil {
				return
			}
			srv.Serve()
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			ch.Close()
			return
		case "signal":
			if s.ignoreSignal {
				continue
			}
			var payload struct{ Signal string }
			if err := ssh.Unmarshal(req.Payload, &payload); err == nil && cmd != nil && cmd.Process != nil {
				signalProcessGroup(cmd, payload.Signal == "KILL")
			}
		case "env":
			// Accepted silently, like an sshd with AcceptEnv — tests do not
			// depend on it.
			if req.WantReply {
				req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
	if done != nil {
		<-done
	}
}

// GenerateKey returns a fresh ed25519 key pair: the signer for a server's
// authorized list and the private key in OpenSSH PEM form, optionally
// encrypted with passphrase.
func GenerateKey(t testing.TB, passphrase string) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshtest: generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshtest: signer: %v", err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "sshtest")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "sshtest", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("sshtest: marshal key: %v", err)
	}
	return signer, string(pem.EncodeToMemory(block))
}

// KnownHostsLine returns the known_hosts entry for the server, as a user
// would paste it.
func (s *Server) KnownHostsLine() string {
	return fmt.Sprintf("[%s]:%d %s", s.Host, s.Port, string(ssh.MarshalAuthorizedKey(s.HostKey)))
}

// AuthorizedKeyLine returns the server's host key in authorized_keys form,
// the form host_key takes.
func (s *Server) AuthorizedKeyLine() string {
	return string(ssh.MarshalAuthorizedKey(s.HostKey))
}

// Discard drains a reader; helper for tests that only care about completion.
func Discard(r io.Reader) { io.Copy(io.Discard, r) }

// HasHostKey reports whether key is one of the server's host keys.
func (s *Server) HasHostKey(key ssh.PublicKey) bool {
	for _, k := range s.HostKeys {
		if string(k.Marshal()) == string(key.Marshal()) {
			return true
		}
	}
	return false
}

// slowRWC delays each Read and Write — a channel on a slow link.
type slowRWC struct {
	io.ReadWriteCloser
	delay time.Duration
}

func (s *slowRWC) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.ReadWriteCloser.Read(p)
}

func (s *slowRWC) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.ReadWriteCloser.Write(p)
}
