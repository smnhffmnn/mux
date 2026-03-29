package vault

import (
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnConfig configures the FIDO2 relying party.
type WebAuthnConfig struct {
	RPID          string   // e.g. "mux.local" or a real domain
	RPOrigins     []string // e.g. ["https://mux.local:7700"]
	RPDisplayName string   // e.g. "Mux Vault"
}

// WebAuthnServer handles FIDO2 registration and authentication for vault unlock.
type WebAuthnServer struct {
	wa    *webauthn.WebAuthn
	vault *Vault
	user  *vaultUser

	sessionMu    sync.Mutex
	regSessions  map[string]*webauthn.SessionData // keyed by challenge
	authSessions map[string]*webauthn.SessionData // keyed by challenge
}

// NewWebAuthnServer creates a WebAuthn server tied to a vault.
func NewWebAuthnServer(cfg WebAuthnConfig, v *Vault) (*WebAuthnServer, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("init webauthn: %w", err)
	}

	user := &vaultUser{
		id:          []byte("mux-vault-owner"),
		name:        "vault-owner",
		displayName: "Vault Owner",
		vault:       v,
	}

	return &WebAuthnServer{
		wa:           wa,
		vault:        v,
		user:         user,
		regSessions:  make(map[string]*webauthn.SessionData),
		authSessions: make(map[string]*webauthn.SessionData),
	}, nil
}

// BeginRegistration starts a new credential registration ceremony.
// The vault must be unlocked (authenticated user adding a new authenticator).
func (s *WebAuthnServer) BeginRegistration() ([]byte, error) {
	if s.vault.State() != StateUnlocked {
		return nil, fmt.Errorf("vault must be unlocked to register credentials")
	}

	options, session, err := s.wa.BeginRegistration(s.user)
	if err != nil {
		return nil, fmt.Errorf("begin registration: %w", err)
	}

	s.sessionMu.Lock()
	s.regSessions[session.Challenge] = session
	s.sessionMu.Unlock()

	return marshalJSON(options)
}

// FinishRegistration completes registration with the authenticator's response.
// Returns the credential name/ID on success.
func (s *WebAuthnServer) FinishRegistration(body []byte, name string) (*StoredCredential, error) {
	// Find the matching session. The go-webauthn library validates the challenge
	// from the response against the session, so we try all pending sessions.
	// In practice there is usually only one.
	s.sessionMu.Lock()
	var session *webauthn.SessionData
	for k, sess := range s.regSessions {
		session = sess
		delete(s.regSessions, k)
		break
	}
	s.sessionMu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("no pending registration session")
	}

	parsedResponse, err := parseCredentialCreationResponse(body)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	credential, err := s.wa.CreateCredential(s.user, *session, parsedResponse)
	if err != nil {
		return nil, fmt.Errorf("verify registration: %w", err)
	}

	sc := StoredCredential{
		ID:        b64Encode(credential.ID),
		PublicKey: b64Encode(credential.PublicKey),
		AAGUID:    hex.EncodeToString(credential.Authenticator.AAGUID),
		SignCount: credential.Authenticator.SignCount,
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.vault.AddCredential(sc); err != nil {
		return nil, err
	}

	log.Printf("[vault] WebAuthn credential registered: %s", name)
	return &sc, nil
}

// BeginLogin starts an authentication ceremony.
func (s *WebAuthnServer) BeginLogin() ([]byte, error) {
	if s.vault.State() == StateUninitialized {
		return nil, fmt.Errorf("vault not initialized")
	}

	creds := s.vault.Credentials()
	if len(creds) == 0 {
		return nil, fmt.Errorf("no credentials registered — unlock with passphrase first, then register a credential")
	}

	options, session, err := s.wa.BeginLogin(s.user)
	if err != nil {
		return nil, fmt.Errorf("begin login: %w", err)
	}

	s.sessionMu.Lock()
	s.authSessions[session.Challenge] = session
	s.sessionMu.Unlock()

	return marshalJSON(options)
}

// FinishLogin completes authentication. On success, unlocks the vault via auth key.
// Returns a session token that must be presented to grant approvals.
func (s *WebAuthnServer) FinishLogin(body []byte) (string, error) {
	s.sessionMu.Lock()
	var session *webauthn.SessionData
	for k, sess := range s.authSessions {
		session = sess
		delete(s.authSessions, k)
		break
	}
	s.sessionMu.Unlock()

	if session == nil {
		return "", fmt.Errorf("no pending login session")
	}

	parsedResponse, err := parseCredentialAssertionResponse(body)
	if err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	credential, err := s.wa.ValidateLogin(s.user, *session, parsedResponse)
	if err != nil {
		return "", fmt.Errorf("authentication failed: %w", err)
	}

	// Update sign count
	credID := b64Encode(credential.ID)
	if err := s.vault.UpdateCredentialSignCount(credID, credential.Authenticator.SignCount); err != nil {
		log.Printf("[vault] Warning: failed to update sign count: %v", err)
	}

	// Unlock vault via auth key
	if err := s.vault.UnlockWithAuthKey(); err != nil {
		return "", fmt.Errorf("vault unlock after auth: %w", err)
	}

	// Issue session token for approval grant authentication
	token, err := s.vault.IssueSessionToken()
	if err != nil {
		return "", fmt.Errorf("issue session token: %w", err)
	}

	log.Printf("[vault] WebAuthn authentication successful, vault unlocked")
	return token, nil
}

// --- vaultUser implements webauthn.User ---

type vaultUser struct {
	id          []byte
	name        string
	displayName string
	vault       *Vault
}

func (u *vaultUser) WebAuthnID() []byte          { return u.id }
func (u *vaultUser) WebAuthnName() string         { return u.name }
func (u *vaultUser) WebAuthnDisplayName() string   { return u.displayName }

func (u *vaultUser) WebAuthnCredentials() []webauthn.Credential {
	stored := u.vault.Credentials()
	creds := make([]webauthn.Credential, 0, len(stored))
	for _, sc := range stored {
		id, err := b64Decode(sc.ID)
		if err != nil {
			continue
		}
		pk, err := b64Decode(sc.PublicKey)
		if err != nil {
			continue
		}
		aaguidBytes, _ := hex.DecodeString(sc.AAGUID)
		if len(aaguidBytes) < 16 {
			padded := make([]byte, 16)
			copy(padded, aaguidBytes)
			aaguidBytes = padded
		}
		creds = append(creds, webauthn.Credential{
			ID:              id,
			PublicKey:        pk,
			AttestationType: "none",
			Authenticator: webauthn.Authenticator{
				AAGUID:    aaguidBytes,
				SignCount: sc.SignCount,
			},
		})
	}
	return creds
}
