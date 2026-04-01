package vault

import (
	"crypto"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	defaultKeyLifetime = 120 // seconds
	vaultSSHSecretKey  = "ssh-private-key"
	sshKeyComment      = "mux-vault-managed"
)

// SSHKeyStatus reports the state of the managed SSH key.
type SSHKeyStatus struct {
	Stored        bool   `json:"stored"`          // key exists in vault
	LoadedInAgent bool   `json:"loaded_in_agent"` // key is currently in ssh-agent
	Fingerprint   string `json:"fingerprint,omitempty"`
}

// SSHManager handles SSH key storage in the vault and injection into ssh-agent.
type SSHManager struct {
	vault *Vault
}

// NewSSHManager creates a manager tied to a vault.
func NewSSHManager(v *Vault) *SSHManager {
	return &SSHManager{vault: v}
}

// Status reports whether the SSH key is stored and/or loaded.
func (m *SSHManager) Status() SSHKeyStatus {
	status := SSHKeyStatus{}

	if m.vault.State() != StateUnlocked {
		return status
	}

	raw, err := m.vault.GetSecret(vaultSSHSecretKey)
	if err != nil || raw == "" {
		return status
	}
	status.Stored = true

	// Parse to get fingerprint
	signer, err := ssh.ParsePrivateKey([]byte(raw))
	if err == nil {
		status.Fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
	}

	// Check if loaded in agent
	agentConn, err := dialAgent()
	if err != nil {
		return status
	}
	defer agentConn.Close()

	agentClient := agent.NewClient(agentConn)
	keys, err := agentClient.List()
	if err != nil {
		return status
	}

	for _, k := range keys {
		if k.Comment == sshKeyComment {
			status.LoadedInAgent = true
			break
		}
	}

	return status
}

// Load reads the SSH key from the vault and adds it to ssh-agent with a lifetime.
// Returns the key fingerprint on success.
func (m *SSHManager) Load(lifetimeSecs uint32) (string, error) {
	if m.vault.State() != StateUnlocked {
		return "", fmt.Errorf("vault must be unlocked")
	}

	if lifetimeSecs == 0 {
		lifetimeSecs = defaultKeyLifetime
	}

	raw, err := m.vault.GetSecret(vaultSSHSecretKey)
	if err != nil || raw == "" {
		return "", fmt.Errorf("no SSH key stored in vault (key: %s)", vaultSSHSecretKey)
	}

	// Parse the private key
	rawKey, err := ssh.ParseRawPrivateKey([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("parse SSH key: %w", err)
	}

	// Get fingerprint via signer
	signer, err := ssh.NewSignerFromKey(rawKey)
	if err != nil {
		return "", fmt.Errorf("create signer: %w", err)
	}
	fingerprint := ssh.FingerprintSHA256(signer.PublicKey())

	// Connect to ssh-agent
	agentConn, err := dialAgent()
	if err != nil {
		return "", fmt.Errorf("connect to ssh-agent: %w", err)
	}
	defer agentConn.Close()

	agentClient := agent.NewClient(agentConn)

	// Add key with lifetime (auto-expires)
	err = agentClient.Add(agent.AddedKey{
		PrivateKey:   rawKey.(crypto.PrivateKey),
		Comment:      sshKeyComment,
		LifetimeSecs: lifetimeSecs,
	})
	if err != nil {
		return "", fmt.Errorf("add key to agent: %w", err)
	}

	return fingerprint, nil
}

// Unload removes the managed SSH key from the agent.
func (m *SSHManager) Unload() error {
	raw, err := m.vault.GetSecret(vaultSSHSecretKey)
	if err != nil || raw == "" {
		return fmt.Errorf("no SSH key stored in vault")
	}

	signer, err := ssh.ParsePrivateKey([]byte(raw))
	if err != nil {
		return fmt.Errorf("parse SSH key: %w", err)
	}

	agentConn, err := dialAgent()
	if err != nil {
		return fmt.Errorf("connect to ssh-agent: %w", err)
	}
	defer agentConn.Close()

	agentClient := agent.NewClient(agentConn)
	return agentClient.Remove(signer.PublicKey())
}

// Import stores an SSH private key in the vault.
// Accepts either raw PEM key data or a file path (for localhost use only).
// If the key has a passphrase, it must be provided.
// Returns the public key fingerprint.
func (m *SSHManager) Import(keyDataOrPath, passphrase string) (string, error) {
	if m.vault.State() != StateUnlocked {
		return "", fmt.Errorf("vault must be unlocked")
	}

	var data []byte

	// Check if input looks like a PEM key (starts with -----BEGIN)
	if len(keyDataOrPath) > 10 && keyDataOrPath[:10] == "-----BEGIN" {
		data = []byte(keyDataOrPath)
	} else {
		// Treat as file path — validate it's under ~/.ssh/ (after resolving traversals)
		keyPath := filepath.Clean(keyDataOrPath)
		home, _ := os.UserHomeDir()
		sshDir := filepath.Clean(home+"/.ssh") + "/"
		if home == "" || !hasPrefix(keyPath, sshDir) {
			return "", fmt.Errorf("key_path must be under ~/.ssh/ (got: %s)", keyPath)
		}

		var err error
		data, err = os.ReadFile(keyPath)
		if err != nil {
			return "", fmt.Errorf("read key file: %w", err)
		}
		// Sanity check file size (SSH keys are typically < 16 KB)
		if len(data) > 16*1024 {
			return "", fmt.Errorf("key file too large (%d bytes, max 16 KB)", len(data))
		}
	}

	// Validate the key parses correctly
	var (
		signer ssh.Signer
		err    error
	)
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(data)
	}
	if err != nil {
		return "", fmt.Errorf("parse key: %w", err)
	}

	fingerprint := ssh.FingerprintSHA256(signer.PublicKey())

	// If key was passphrase-protected, re-encode without passphrase for vault storage.
	// The vault encryption replaces the passphrase protection.
	if passphrase != "" {
		rawKey, err := ssh.ParseRawPrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			return "", fmt.Errorf("decrypt key: %w", err)
		}
		pemBlock, err := ssh.MarshalPrivateKey(rawKey.(crypto.PrivateKey), sshKeyComment)
		if err != nil {
			return "", fmt.Errorf("marshal key: %w", err)
		}
		data = pem.EncodeToMemory(pemBlock)
	}

	// Store in vault
	if err := m.vault.SetSecret(vaultSSHSecretKey, string(data)); err != nil {
		return "", fmt.Errorf("store in vault: %w", err)
	}

	return fingerprint, nil
}

// PublicKey returns the public key in authorized_keys format (for adding to GitLab/GitHub).
func (m *SSHManager) PublicKey() (string, error) {
	if m.vault.State() != StateUnlocked {
		return "", fmt.Errorf("vault must be unlocked")
	}

	raw, err := m.vault.GetSecret(vaultSSHSecretKey)
	if err != nil || raw == "" {
		return "", fmt.Errorf("no SSH key stored in vault")
	}

	signer, err := ssh.ParsePrivateKey([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("parse key: %w", err)
	}

	pubKey := signer.PublicKey()
	ak := ssh.MarshalAuthorizedKey(pubKey)
	// MarshalAuthorizedKey appends a newline — trim it and add our comment
	return fmt.Sprintf("%s %s", string(ak[:len(ak)-1]), sshKeyComment), nil
}

// hasPrefix checks if path starts with the given directory prefix.
func hasPrefix(path, prefix string) bool {
	return len(path) >= len(prefix) && path[:len(prefix)] == prefix
}

// dialAgent connects to the SSH agent socket.
func dialAgent() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set — is ssh-agent running?")
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sock, err)
	}
	return conn, nil
}
