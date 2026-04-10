//go:build !windows

package vault

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	credentialSocketName    = "credential.sock"
	credentialReadTimeout   = 5 * time.Second
	credentialMaxRequestLen = 4096
)

// GitHost describes a git credential source derived from a connection config.
type GitHost struct {
	Host      string // e.g. "gitlab.com"
	Username  string // e.g. "oauth2"
	SecretKey string // vault secret name, e.g. "gitlab-token"
}

// credentialRequest is sent by the mux git-credential subcommand.
type credentialRequest struct {
	Host string `json:"host"`
}

// credentialResponse is returned to the subcommand.
type credentialResponse struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// CredentialSocket serves git credential requests over a Unix domain socket.
// Only responds to hosts configured as git connections. Secrets never leave localhost.
type CredentialSocket struct {
	vault    *Vault
	hosts    map[string]GitHost // host → config
	listener net.Listener
	wg       sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
}

// NewCredentialSocket creates a new credential socket backed by the given vault.
// gitHosts defines which hosts are served and how to look up their credentials.
func NewCredentialSocket(v *Vault, gitHosts []GitHost) *CredentialSocket {
	hostMap := make(map[string]GitHost, len(gitHosts))
	for _, gh := range gitHosts {
		hostMap[gh.Host] = gh
	}
	return &CredentialSocket{
		vault:  v,
		hosts:  hostMap,
		closed: make(chan struct{}),
	}
}

// SocketPath returns the path to the Unix domain socket.
func SocketPath() string {
	return filepath.Join(vaultDir(), credentialSocketName)
}

// Listen starts accepting connections on the Unix socket.
// Blocks until Close is called or a fatal error occurs.
func (cs *CredentialSocket) Listen() error {
	sockPath := SocketPath()

	// Remove stale socket from previous run (only if it's actually a socket)
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&os.ModeSocket != 0 {
			os.Remove(sockPath)
		} else {
			return fmt.Errorf("credential socket: %s exists but is not a socket", sockPath)
		}
	}

	// Set restrictive umask before creating socket to prevent TOCTOU race.
	// Between net.Listen and os.Chmod, the socket would briefly have default
	// permissions. Setting umask 0077 ensures it's created as 0700 from the start.
	oldUmask := syscall.Umask(0077)
	ln, err := net.Listen("unix", sockPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return fmt.Errorf("credential socket listen: %w", err)
	}
	cs.listener = ln

	// Explicitly set to 0600 for clarity (umask already restricts, but be explicit)
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("credential socket chmod: %w", err)
	}

	log.Printf("[vault] Credential socket listening: %s (%d git hosts configured)", sockPath, len(cs.hosts))

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-cs.closed:
				return nil // clean shutdown
			default:
				log.Printf("[vault] Credential socket accept error: %v", err)
				continue
			}
		}
		cs.wg.Add(1)
		go cs.handleConn(conn)
	}
}

// Close stops the listener and waits for active connections to finish.
// Safe to call multiple times.
func (cs *CredentialSocket) Close() {
	cs.closeOnce.Do(func() {
		close(cs.closed)
		if cs.listener != nil {
			cs.listener.Close()
		}
		cs.wg.Wait()
		os.Remove(SocketPath())
	})
}

func (cs *CredentialSocket) handleConn(conn net.Conn) {
	defer cs.wg.Done()
	defer conn.Close()

	// Deadline prevents goroutine leaks from stalled/malicious clients.
	conn.SetDeadline(time.Now().Add(credentialReadTimeout))

	var req credentialRequest
	if err := json.NewDecoder(io.LimitReader(conn, credentialMaxRequestLen)).Decode(&req); err != nil {
		cs.writeResponse(conn, credentialResponse{Error: "invalid request"})
		return
	}

	// Only serve credentials for configured git connections
	gh, ok := cs.hosts[req.Host]
	if !ok {
		cs.writeResponse(conn, credentialResponse{Error: "no git connection for requested host"})
		return
	}

	token, err := cs.vault.GetSecret(gh.SecretKey)
	if err != nil {
		// Log the full error server-side but return a generic message to the client.
		// Vault errors may contain internal paths or crypto details.
		log.Printf("[vault] Credential socket: secret %q: %v", gh.SecretKey, err)
		cs.writeResponse(conn, credentialResponse{Error: "secret unavailable"})
		return
	}

	cs.writeResponse(conn, credentialResponse{
		Username: gh.Username,
		Password: token,
	})
}

func (cs *CredentialSocket) writeResponse(conn net.Conn, resp credentialResponse) {
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		log.Printf("[vault] Credential socket write error: %v", err)
	}
}
