package vault

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// State represents the vault lifecycle.
type State string

const (
	StateUninitialized State = "uninitialized" // no vault file
	StateSealed        State = "sealed"        // vault exists, DEK not in memory
	StateUnlocked      State = "unlocked"      // DEK in memory, secrets accessible
)

const DefaultInactivityTimeout = 30 * time.Minute

// Vault manages encrypted secrets with WebAuthn-gated access.
type Vault struct {
	mu    sync.RWMutex
	store *StoreFile

	// DEK — only present while unlocked, wiped on lock
	dek []byte

	// Inactivity auto-lock
	inactivityTimeout time.Duration
	lastActivity      atomic.Int64 // UnixNano — atomic to avoid races with RLock readers
	timerStop         chan struct{}

	// Optional callback on auto-lock (e.g. notify frontend)
	onLock func()

	// Session token issued by WebAuthn login, required by approval grant.
	// 128-bit random hex, rotated on each successful WebAuthn login.
	// Ensures the grant endpoint cannot be called without completing WebAuthn.
	sessionToken   atomic.Value // string
	sessionTokenAt atomic.Int64 // UnixNano when token was issued
}

// Option configures the vault.
type Option func(*Vault)

func WithInactivityTimeout(d time.Duration) Option {
	return func(v *Vault) { v.inactivityTimeout = d }
}

func WithOnLock(fn func()) Option {
	return func(v *Vault) { v.onLock = fn }
}

// New creates a Vault. Call Load() to read existing state from disk.
func New(opts ...Option) *Vault {
	v := &Vault{
		inactivityTimeout: DefaultInactivityTimeout,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Load reads vault state from disk.
func (v *Vault) Load() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	store, err := loadStore()
	if err != nil {
		return err
	}
	v.store = store // nil if no vault file exists
	return nil
}

// State returns the current vault state.
func (v *Vault) State() State {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.stateLocked()
}

// IsUnlocked reports whether the vault is unlocked.
// Implements config.SecretVault interface.
func (v *Vault) IsUnlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dek != nil
}

// Init creates a new vault with the given passphrase.
// The vault is unlocked after Init.
func (v *Vault) Init(passphrase string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store != nil {
		return fmt.Errorf("vault already initialized")
	}

	// Generate random DEK
	dek, err := generateKey()
	if err != nil {
		return err
	}

	// Wrap DEK with passphrase (Argon2id)
	salt, err := generateSalt()
	if err != nil {
		wipeBytes(dek)
		return err
	}
	wrappingKey := deriveKey(passphrase, salt)
	wrappedDEK, err := encrypt(wrappingKey, dek)
	wipeBytes(wrappingKey)
	if err != nil {
		wipeBytes(dek)
		return fmt.Errorf("wrap DEK with passphrase: %w", err)
	}

	// Generate auth key and wrap DEK with it (for WebAuthn unlock)
	authKey, err := generateKey()
	if err != nil {
		wipeBytes(dek)
		return err
	}
	authWrappedDEK, err := encrypt(authKey, dek)
	if err != nil {
		wipeBytes(dek)
		wipeBytes(authKey)
		return fmt.Errorf("wrap DEK with auth key: %w", err)
	}
	if err := saveAuthKey(authKey); err != nil {
		wipeBytes(dek)
		wipeBytes(authKey)
		return err
	}
	wipeBytes(authKey)

	v.store = &StoreFile{
		Version:   storeVersion,
		CreatedAt: time.Now().UTC(),
		Passphrase: &PassphraseConfig{
			Salt:       b64Encode(salt),
			WrappedDEK: b64Encode(wrappedDEK),
		},
		AuthKey: &AuthKeyConfig{
			WrappedDEK: b64Encode(authWrappedDEK),
		},
		Secrets: make(map[string]*EncryptedValue),
	}

	if err := saveStore(v.store); err != nil {
		wipeBytes(dek)
		v.store = nil
		return err
	}

	v.dek = dek
	v.startTimer()
	log.Printf("[vault] Initialized and unlocked")
	return nil
}

// UnlockWithPassphrase decrypts the DEK using the passphrase.
func (v *Vault) UnlockWithPassphrase(passphrase string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil {
		return fmt.Errorf("vault not initialized")
	}
	if v.dek != nil {
		v.touch()
		return nil // already unlocked
	}
	if v.store.Passphrase == nil {
		return fmt.Errorf("passphrase unlock not configured")
	}

	salt, err := b64Decode(v.store.Passphrase.Salt)
	if err != nil {
		return fmt.Errorf("decode salt: %w", err)
	}

	wrappingKey := deriveKey(passphrase, salt)
	wrappedDEK, err := b64Decode(v.store.Passphrase.WrappedDEK)
	if err != nil {
		wipeBytes(wrappingKey)
		return fmt.Errorf("decode wrapped DEK: %w", err)
	}

	dek, err := decrypt(wrappingKey, wrappedDEK)
	wipeBytes(wrappingKey)
	if err != nil {
		return fmt.Errorf("invalid passphrase")
	}

	v.dek = dek
	v.startTimer()
	log.Printf("[vault] Unlocked via passphrase")
	return nil
}

// UnlockWithAuthKey decrypts the DEK using the auth key from disk.
// Called after successful WebAuthn authentication.
func (v *Vault) UnlockWithAuthKey() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.store == nil {
		return fmt.Errorf("vault not initialized")
	}
	if v.dek != nil {
		v.touch()
		return nil
	}
	if v.store.AuthKey == nil {
		return fmt.Errorf("auth key unlock not configured")
	}

	authKey, err := loadAuthKey()
	if err != nil {
		return fmt.Errorf("load auth key: %w", err)
	}

	wrappedDEK, err := b64Decode(v.store.AuthKey.WrappedDEK)
	if err != nil {
		wipeBytes(authKey)
		return fmt.Errorf("decode wrapped DEK: %w", err)
	}

	dek, err := decrypt(authKey, wrappedDEK)
	wipeBytes(authKey)
	if err != nil {
		return fmt.Errorf("auth key mismatch or corrupted vault")
	}

	v.dek = dek
	v.startTimer()
	log.Printf("[vault] Unlocked via auth key (WebAuthn)")
	return nil
}

// Lock wipes the DEK from memory, sealing the vault.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lockInternal()
}

func (v *Vault) lockInternal() {
	if v.dek != nil {
		wipeBytes(v.dek)
		v.dek = nil
		v.sessionToken.Store("")  // invalidate session token on lock
		v.sessionTokenAt.Store(0)
		log.Printf("[vault] Locked")
	}
	v.stopTimer()
}

// --- Secrets ---

// GetSecret decrypts and returns a secret. Vault must be unlocked.
func (v *Vault) GetSecret(key string) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dek == nil {
		return "", fmt.Errorf("vault is sealed")
	}

	ev, ok := v.store.Secrets[key]
	if !ok {
		return "", fmt.Errorf("secret %q not found in vault", key)
	}

	data, err := b64Decode(ev.Data)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}

	plaintext, err := decrypt(v.dek, data)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", key, err)
	}

	// Copy to string before wiping the byte slice.
	// The string is immutable and cannot be wiped (Go limitation),
	// but we minimize the window where both copies exist.
	result := string(plaintext)
	wipeBytes(plaintext)

	v.touch()
	return result, nil
}

// SetSecret encrypts and stores a secret. Vault must be unlocked.
func (v *Vault) SetSecret(key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dek == nil {
		return fmt.Errorf("vault is sealed")
	}

	data, err := encrypt(v.dek, []byte(value))
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}

	v.store.Secrets[key] = &EncryptedValue{Data: b64Encode(data)}
	v.touch()
	return saveStore(v.store)
}

// DeleteSecret removes a secret. Vault must be unlocked.
func (v *Vault) DeleteSecret(key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dek == nil {
		return fmt.Errorf("vault is sealed")
	}

	if _, ok := v.store.Secrets[key]; !ok {
		return fmt.Errorf("secret %q not found in vault", key)
	}

	delete(v.store.Secrets, key)
	v.touch()
	return saveStore(v.store)
}

// HasSecret checks if a key exists (works even when sealed).
func (v *Vault) HasSecret(key string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.store == nil {
		return false
	}
	_, ok := v.store.Secrets[key]
	return ok
}

// SecretKeys returns all stored key names (works even when sealed).
func (v *Vault) SecretKeys() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.store == nil {
		return nil
	}
	keys := make([]string, 0, len(v.store.Secrets))
	for k := range v.store.Secrets {
		keys = append(keys, k)
	}
	return keys
}

// --- WebAuthn Credentials ---

// Credentials returns stored WebAuthn credentials.
func (v *Vault) Credentials() []StoredCredential {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.store == nil {
		return nil
	}
	creds := make([]StoredCredential, len(v.store.Credentials))
	copy(creds, v.store.Credentials)
	return creds
}

// AddCredential stores a new WebAuthn credential.
func (v *Vault) AddCredential(cred StoredCredential) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.store == nil {
		return fmt.Errorf("vault not initialized")
	}
	v.store.Credentials = append(v.store.Credentials, cred)
	return saveStore(v.store)
}

// UpdateCredentialSignCount bumps the counter after successful auth.
func (v *Vault) UpdateCredentialSignCount(credID string, count uint32) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.store == nil {
		return fmt.Errorf("vault not initialized")
	}
	for i := range v.store.Credentials {
		if v.store.Credentials[i].ID == credID {
			v.store.Credentials[i].SignCount = count
			return saveStore(v.store)
		}
	}
	return fmt.Errorf("credential %q not found", credID)
}

// RemoveCredential deletes a WebAuthn credential by ID.
func (v *Vault) RemoveCredential(credID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.store == nil {
		return fmt.Errorf("vault not initialized")
	}
	var filtered []StoredCredential
	found := false
	for _, c := range v.store.Credentials {
		if c.ID == credID {
			found = true
			continue
		}
		filtered = append(filtered, c)
	}
	if !found {
		return fmt.Errorf("credential %q not found", credID)
	}
	v.store.Credentials = filtered
	return saveStore(v.store)
}

// --- Inactivity Timer ---

func (v *Vault) touch() {
	v.lastActivity.Store(time.Now().UnixNano())
}

func (v *Vault) sinceLastActivity() time.Duration {
	last := v.lastActivity.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

func (v *Vault) startTimer() {
	v.stopTimer()
	v.touch()
	v.timerStop = make(chan struct{})
	go v.timerLoop(v.timerStop)
}

func (v *Vault) stopTimer() {
	if v.timerStop != nil {
		close(v.timerStop)
		v.timerStop = nil
	}
}

func (v *Vault) timerLoop(stop chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			v.mu.Lock()
			if v.dek != nil && v.sinceLastActivity() > v.inactivityTimeout {
				v.lockInternal()
				// Read callback while holding lock to avoid race
				cb := v.onLock
				v.mu.Unlock()
				if cb != nil {
					cb()
				}
				log.Printf("[vault] Auto-locked after %v of inactivity", v.inactivityTimeout)
				return
			}
			v.mu.Unlock()
		}
	}
}

// --- Status ---

// Status returns a summary for display/MCP. Never exposes secret values.
type Status struct {
	State           State    `json:"state"`
	SecretCount     int      `json:"secret_count"`
	CredentialCount int      `json:"credential_count"`
	CredentialNames []string `json:"credential_names,omitempty"`
	InactivityLeft  string   `json:"inactivity_left,omitempty"` // only when unlocked
}

// stateLocked returns the state assuming the caller holds at least an RLock.
func (v *Vault) stateLocked() State {
	if v.store == nil {
		return StateUninitialized
	}
	if v.dek != nil {
		return StateUnlocked
	}
	return StateSealed
}

func (v *Vault) Status() Status {
	v.mu.RLock()
	defer v.mu.RUnlock()

	s := Status{State: v.stateLocked()}

	if v.store == nil {
		return s
	}

	s.SecretCount = len(v.store.Secrets)
	s.CredentialCount = len(v.store.Credentials)
	for _, c := range v.store.Credentials {
		s.CredentialNames = append(s.CredentialNames, c.Name)
	}

	if v.dek != nil && v.inactivityTimeout > 0 {
		remaining := v.inactivityTimeout - v.sinceLastActivity()
		if remaining < 0 {
			remaining = 0
		}
		s.InactivityLeft = remaining.Truncate(time.Second).String()
	}

	return s
}

// --- Session Tokens (for approval grant authentication) ---

const sessionTokenTTL = 5 * time.Minute

// IssueSessionToken generates a new session token (called after WebAuthn login).
// Returns the token that must be presented to grant an approval.
// Tokens expire after 5 minutes.
func (v *Vault) IssueSessionToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := hex.EncodeToString(b)
	v.sessionToken.Store(token)
	v.sessionTokenAt.Store(time.Now().UnixNano())
	return token, nil
}

// ValidateSessionToken checks if the given token matches the current session.
// Uses constant-time comparison to prevent timing side-channels.
// Rejects tokens older than sessionTokenTTL.
func (v *Vault) ValidateSessionToken(token string) bool {
	current, ok := v.sessionToken.Load().(string)
	if !ok || current == "" || len(current) != len(token) {
		return false
	}
	// Check TTL
	issuedAt := v.sessionTokenAt.Load()
	if issuedAt == 0 || time.Since(time.Unix(0, issuedAt)) > sessionTokenTTL {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(current), []byte(token)) == 1
}
