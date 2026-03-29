package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"
)

// SecretVault is the interface that the vault package satisfies.
// Defined here to avoid importing the vault package (which would create
// an upward dependency from config into a higher-level subsystem).
type SecretVault interface {
	IsUnlocked() bool
	GetSecret(key string) (string, error)
	SetSecret(key, value string) error
	DeleteSecret(key string) error
}

// secretsFile holds the TOML structure for file-based secret storage.
// Used as fallback when the OS keyring is unavailable (headless Linux, SSH, after reboot).
type secretsFile struct {
	Secrets map[string]string `toml:"secrets"`
}

var (
	fileStoreMu   sync.Mutex
	keyringBroken atomic.Bool // cached: true after first keyring failure

	// activeVault is set when the vault feature is enabled.
	// When set, getSecret/setSecret/deleteSecret try the vault first.
	activeVaultMu   sync.RWMutex
	activeVaultInst SecretVault

	// vaultExclusive, when true, skips keyring/file writes for secrets
	// that are successfully stored in the vault. Eliminates the plaintext
	// copy that otherwise undermines encryption at rest.
	vaultExclusive atomic.Bool
)

// SetActiveVault registers the vault for the secrets layer.
// When set, all secret operations try the vault first, then fall back
// to keyring/file for secrets not yet migrated.
func SetActiveVault(v SecretVault) {
	activeVaultMu.Lock()
	activeVaultInst = v
	activeVaultMu.Unlock()
}

// getActiveVault returns the current vault (or nil).
func getActiveVault() SecretVault {
	activeVaultMu.RLock()
	v := activeVaultInst
	activeVaultMu.RUnlock()
	return v
}

// SetVaultExclusive controls whether vault-stored secrets are also written
// to legacy stores (keyring/file). When true, a successful vault write
// skips the legacy stores — secrets exist only encrypted in the vault.
func SetVaultExclusive(exclusive bool) {
	vaultExclusive.Store(exclusive)
}

// secretsFilePath returns ~/.mux/secrets.toml.
func secretsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, "secrets.toml")
}

// getSecret reads a secret. Priority: vault (if enabled+unlocked) → keyring → file.
func getSecret(key string) (string, error) {
	// Try vault first
	if v := getActiveVault(); v != nil && v.IsUnlocked() {
		if val, err := v.GetSecret(key); err == nil {
			return val, nil
		}
		// Secret not in vault — fall through to legacy stores
	}

	if !keyringBroken.Load() {
		v, err := keyring.Get(ServiceName, key)
		if err == nil {
			return v, nil
		}
		// "Not found" is not broken — the key just doesn't exist in keyring.
		// Only mark broken for systemic errors (no provider, D-Bus failure, etc.)
		if !isKeyNotFound(err) {
			log.Printf("[secrets] Keyring unavailable (%v), using file fallback", err)
			keyringBroken.Store(true)
		}
	}

	// File fallback (also checked when key is not in keyring — it may have
	// been written during a previous session where keyring was broken)
	return fileGet(key)
}

// setSecret stores a secret. If vault is unlocked, writes to vault.
// In exclusive mode, a successful vault write skips legacy stores.
func setSecret(key, value string) error {
	// Write to vault if available and unlocked
	vaultOK := false
	if v := getActiveVault(); v != nil && v.IsUnlocked() {
		if err := v.SetSecret(key, value); err != nil {
			log.Printf("[secrets] Vault write failed for %q: %v (falling through to keyring/file)", key, err)
		} else {
			vaultOK = true
		}
	}

	// In exclusive mode, skip legacy stores when vault write succeeded
	if vaultOK && vaultExclusive.Load() {
		return nil
	}

	if !keyringBroken.Load() {
		err := keyring.Set(ServiceName, key, value)
		if err != nil && !isKeyNotFound(err) {
			log.Printf("[secrets] Keyring unavailable (%v), using file fallback", err)
			keyringBroken.Store(true)
		}
	}

	// Always write to file — ensures secrets survive keyring loss
	return fileSet(key, value)
}

// deleteSecret removes a secret from vault, keyring, and file store.
func deleteSecret(key string) error {
	// Delete from vault if available
	if v := getActiveVault(); v != nil && v.IsUnlocked() {
		if err := v.DeleteSecret(key); err != nil {
			log.Printf("[secrets] Vault delete failed for %q: %v", key, err)
		}
	}

	// Try both legacy stores — secret might be in either or both.
	var keyringErr, fileErr error

	if !keyringBroken.Load() {
		keyringErr = keyring.Delete(ServiceName, key)
		if keyringErr != nil && !isKeyNotFound(keyringErr) {
			keyringBroken.Store(true)
		}
	}

	fileErr = fileDelete(key)

	// Success if either succeeded (or key didn't exist)
	if keyringErr != nil && !isKeyNotFound(keyringErr) && fileErr != nil {
		return fmt.Errorf("delete from keyring: %w; delete from file: %v", keyringErr, fileErr)
	}
	return nil
}

// isKeyNotFound returns true if the error means "no such key" (not a systemic failure).
func isKeyNotFound(err error) bool {
	return err == keyring.ErrNotFound
}

// --- File-based store ---

func fileGet(key string) (string, error) {
	fileStoreMu.Lock()
	defer fileStoreMu.Unlock()

	sf := loadSecretsFile()
	v, ok := sf.Secrets[key]
	if !ok {
		return "", fmt.Errorf("secret %q not found", key)
	}
	return v, nil
}

func fileSet(key, value string) error {
	fileStoreMu.Lock()
	defer fileStoreMu.Unlock()

	sf := loadSecretsFile()
	sf.Secrets[key] = value
	return writeSecretsFile(sf)
}

func fileDelete(key string) error {
	fileStoreMu.Lock()
	defer fileStoreMu.Unlock()

	sf := loadSecretsFile()
	if _, ok := sf.Secrets[key]; !ok {
		return fmt.Errorf("secret %q not found", key)
	}
	delete(sf.Secrets, key)
	return writeSecretsFile(sf)
}

func loadSecretsFile() *secretsFile {
	sf := &secretsFile{Secrets: make(map[string]string)}
	path := secretsFilePath()
	if _, err := os.Stat(path); err != nil {
		return sf
	}
	if _, err := toml.DecodeFile(path, sf); err != nil {
		log.Printf("[secrets] Warning: could not read %s: %v", path, err)
		return &secretsFile{Secrets: make(map[string]string)}
	}
	if sf.Secrets == nil {
		sf.Secrets = make(map[string]string)
	}
	return sf
}

func writeSecretsFile(sf *secretsFile) error {
	path := secretsFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	// Write to temp file, then rename for atomicity
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create secrets file: %w", err)
	}

	encErr := toml.NewEncoder(f).Encode(sf)
	closeErr := f.Close()

	if encErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write secrets: %w", encErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("flush secrets file: %w", closeErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename secrets file: %w", err)
	}

	return nil
}
