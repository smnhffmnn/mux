package sshclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Auth values: where the private key lives.
const (
	// AuthKey uses a private key mux holds — from the secret store or a file.
	// The default.
	AuthKey = "key"
	// AuthAgent signs through the user's ssh-agent (SSH_AUTH_SOCK). No key
	// material enters mux; the agent decides, and an agent that is locked or
	// absent means the connection is down. On a desktop this is the unlocked
	// agent of the logged-in user; a container has none.
	AuthAgent = "agent"
)

// ValidAuth reports whether auth is a known value ("" counts as AuthKey).
func ValidAuth(auth string) bool {
	return auth == "" || auth == AuthKey || auth == AuthAgent
}

// KeySource says how a connection or tunnel authenticates.
type KeySource struct {
	// Auth selects AuthKey or AuthAgent; "" means AuthKey.
	Auth string
	// PrivateKey is the key in PEM (OpenSSH or PKCS) form, usually read from
	// the secret store. It may be passphrase-protected.
	PrivateKey string
	// Passphrase decrypts PrivateKey when the key is encrypted. Ignored for an
	// unencrypted key, so a stale passphrase does no harm.
	Passphrase string
	// KeyFile is a path to read the key from when PrivateKey is empty. Callers
	// expand ~ before passing it.
	KeyFile string
	// SecretName is the secret-store key that holds (or should hold) the
	// private key, e.g. "tunnel-bastion-private-key". It only appears in error
	// messages, which name the exact secret to set.
	SecretName string
}

// authSource produces auth methods for one handshake. The key variant parsed
// its signer once in resolve; the agent variant talks to the agent at every
// handshake, because the agent's contents change while mux runs.
type authSource interface {
	// methods returns the auth methods plus a cleanup to run once the
	// handshake is over (the agent socket must stay open while the server
	// asks for signatures, and must not stay open longer). ctx bounds the
	// handshake; an agent that waits for the user's confirmation stops
	// blocking when it expires.
	methods(ctx context.Context) ([]ssh.AuthMethod, func(), error)
}

type keyAuth struct{ signer ssh.Signer }

func (k keyAuth) methods(context.Context) ([]ssh.AuthMethod, func(), error) {
	return []ssh.AuthMethod{ssh.PublicKeys(k.signer)}, func() {}, nil
}

type agentAuth struct{ socket string }

func (a agentAuth) methods(ctx context.Context) ([]ssh.AuthMethod, func(), error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", a.socket)
	if err != nil {
		return nil, nil, fmt.Errorf("ssh-agent at %s is not reachable: %w", a.socket, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		// Listing and signing both go over this socket; an agent that asks
		// the user for confirmation ("allow this key?") must not hold every
		// call on the client until someone clicks.
		_ = conn.SetDeadline(deadline)
	}
	ag := agent.NewClient(conn)
	signers, err := ag.Signers()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("ssh-agent: %w", err)
	}
	if len(signers) == 0 {
		conn.Close()
		return nil, nil, errors.New("ssh-agent holds no keys (ssh-add one first)")
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, func() { conn.Close() }, nil
}

// resolve turns the configured source into an authSource, validating what can
// be validated without a network: the key parses (with its passphrase), the
// key file is readable, the agent socket exists.
func (k KeySource) resolve() (authSource, string, error) {
	switch k.Auth {
	case "", AuthKey:
		pemData := k.PrivateKey
		if pemData == "" && k.KeyFile != "" {
			data, err := os.ReadFile(k.KeyFile)
			if err != nil {
				return nil, "", fmt.Errorf("read key_file %s: %w", k.KeyFile, err)
			}
			pemData = string(data)
		}
		if pemData == "" {
			return nil, "", fmt.Errorf("no private key: store it as secret %s or set key_file (or auth = %q for the ssh-agent)", k.secretName(), AuthAgent)
		}
		signer, err := k.parse(pemData)
		if err != nil {
			return nil, "", err
		}
		desc := fmt.Sprintf("key %s %s", signer.PublicKey().Type(), ssh.FingerprintSHA256(signer.PublicKey()))
		return keyAuth{signer}, desc, nil
	case AuthAgent:
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, "", errors.New("auth = agent, but SSH_AUTH_SOCK is not set — no ssh-agent in this environment (containers have none; on Windows the agent is a named pipe, which is not supported)")
		}
		if _, err := os.Stat(sock); err != nil {
			return nil, "", fmt.Errorf("auth = agent, but the socket %s is not there: %w", sock, err)
		}
		return agentAuth{sock}, "ssh-agent", nil
	default:
		return nil, "", fmt.Errorf("auth must be %q or %q, got %q", AuthKey, AuthAgent, k.Auth)
	}
}

// parse reads the PEM key, decrypting it with the passphrase when it needs
// one. The error for a missing passphrase names the secret to set, because
// that is the one thing the user has to do next.
func (k KeySource) parse(pemData string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey([]byte(pemData))
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	if k.Passphrase == "" {
		return nil, fmt.Errorf("private key is passphrase-protected: store the passphrase as secret %s-passphrase", k.secretName())
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(pemData), []byte(k.Passphrase))
	if err != nil {
		return nil, fmt.Errorf("decrypt private key with secret %s-passphrase: %w", k.secretName(), err)
	}
	return signer, nil
}

func (k KeySource) secretName() string {
	if k.SecretName != "" {
		return k.SecretName
	}
	return "<name>-private-key"
}
