package vault

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	vaultFileName   = "vault.json"
	authKeyFileName = "vault.key"
	storeVersion    = 1
)

// StoreFile is the on-disk vault format.
type StoreFile struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`

	// Passphrase-based DEK wrapping
	Passphrase *PassphraseConfig `json:"passphrase,omitempty"`

	// Auth-key-based DEK wrapping (for WebAuthn unlock without passphrase)
	AuthKey *AuthKeyConfig `json:"auth_key,omitempty"`

	// Encrypted secrets (key name -> encrypted value)
	Secrets map[string]*EncryptedValue `json:"secrets"`

	// WebAuthn credentials
	Credentials []StoredCredential `json:"credentials,omitempty"`
}

// PassphraseConfig stores the Argon2id salt and passphrase-wrapped DEK.
type PassphraseConfig struct {
	Salt       string `json:"salt"`        // base64
	WrappedDEK string `json:"wrapped_dek"` // base64, nonce prepended
}

// AuthKeyConfig stores the auth-key-wrapped DEK.
// The auth key itself lives in a separate file (vault.key, chmod 600).
type AuthKeyConfig struct {
	WrappedDEK string `json:"wrapped_dek"` // base64, nonce prepended
}

// EncryptedValue is a secret encrypted with the DEK.
type EncryptedValue struct {
	Data string `json:"data"` // base64, nonce prepended
}

// StoredCredential is a WebAuthn credential persisted in the vault.
type StoredCredential struct {
	ID             string    `json:"id"`              // base64 credential ID
	PublicKey      string    `json:"public_key"`      // base64 COSE public key
	AAGUID         string    `json:"aaguid"`          // hex authenticator GUID
	SignCount      uint32    `json:"sign_count"`
	BackupEligible bool     `json:"backup_eligible"` // passkey can be synced (iCloud, Google)
	BackupState    bool     `json:"backup_state"`    // passkey is currently synced
	Name           string    `json:"name"`            // user-given label (e.g. "iPhone FaceID")
	CreatedAt      time.Time `json:"created_at"`
}

func vaultDir() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return filepath.Join(home, ".mux")
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mux")
	}
	return filepath.Join(home, ".config", "mux")
}

func vaultFilePath() string {
	return filepath.Join(vaultDir(), vaultFileName)
}

func authKeyFilePath() string {
	return filepath.Join(vaultDir(), authKeyFileName)
}

// loadStore reads the vault file from disk. Returns nil,nil if no vault exists.
func loadStore() (*StoreFile, error) {
	data, err := os.ReadFile(vaultFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read vault: %w", err)
	}

	var store StoreFile
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parse vault: %w", err)
	}

	if store.Version != storeVersion {
		return nil, fmt.Errorf("unsupported vault version %d (expected %d)", store.Version, storeVersion)
	}

	if store.Secrets == nil {
		store.Secrets = make(map[string]*EncryptedValue)
	}

	return &store, nil
}

// saveStore writes the vault file atomically (write tmp, rename).
func saveStore(store *StoreFile) error {
	dir := vaultDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create vault dir: %w", err)
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal vault: %w", err)
	}

	path := vaultFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write vault tmp: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename vault: %w", err)
	}

	return nil
}

// loadAuthKey reads the auth key file from disk.
func loadAuthKey() ([]byte, error) {
	encoded, err := os.ReadFile(authKeyFilePath())
	if err != nil {
		return nil, fmt.Errorf("read auth key: %w", err)
	}
	return base64.StdEncoding.DecodeString(string(encoded))
}

// saveAuthKey writes the auth key file (base64 encoded, chmod 600).
func saveAuthKey(key []byte) error {
	dir := vaultDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create vault dir: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(key)
	path := authKeyFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded), 0600); err != nil {
		return fmt.Errorf("write auth key: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename auth key: %w", err)
	}

	return nil
}

// b64Encode encodes bytes to standard base64.
func b64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// b64Decode decodes standard base64.
func b64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
