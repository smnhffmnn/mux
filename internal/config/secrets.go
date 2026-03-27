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

// secretsFile holds the TOML structure for file-based secret storage.
// Used as fallback when the OS keyring is unavailable (headless Linux, SSH, after reboot).
type secretsFile struct {
	Secrets map[string]string `toml:"secrets"`
}

var (
	fileStoreMu   sync.Mutex
	keyringBroken atomic.Bool // cached: true after first keyring failure
)

// secretsFilePath returns ~/.mux/secrets.toml.
func secretsFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, "secrets.toml")
}

// getSecret reads a secret, trying keyring first, then file fallback.
func getSecret(key string) (string, error) {
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

// setSecret stores a secret. Always writes to the file store for durability.
// Also writes to keyring when available (dual-write), so desktop mode has
// instant access without reading the file.
func setSecret(key, value string) error {
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

// deleteSecret removes a secret from keyring and file store.
func deleteSecret(key string) error {
	// Try both — secret might be in either or both.
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
