package vault

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

// VaultHandlers holds pre-built HTTP handlers with shared rate limiters.
// Create once with NewVaultHandlers, then mount on one or more muxes.
type VaultHandlers struct {
	v     *Vault
	wa    *WebAuthnServer
	queue *ApprovalQueue

	passphraseLimiter *RateLimiter
	webauthnLimiter   *RateLimiter
}

// NewVaultHandlers creates handlers with shared rate limiters.
func NewVaultHandlers(v *Vault, wa *WebAuthnServer, queue *ApprovalQueue) *VaultHandlers {
	return &VaultHandlers{
		v:                 v,
		wa:                wa,
		queue:             queue,
		passphraseLimiter: NewRateLimiter(5, 1*time.Minute),
		webauthnLimiter:   NewRateLimiter(5, 1*time.Minute),
	}
}

// Mount registers all vault HTTP endpoints on the given mux.
// Safe to call on multiple muxes — rate limiters are shared.
func (h *VaultHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /vault/status", handleStatus(h.v))
	mux.HandleFunc("POST /vault/init", handleInit(h.v))
	mux.HandleFunc("POST /vault/unlock", h.passphraseLimiter.Wrap(handleUnlock(h.v)))
	mux.HandleFunc("POST /vault/lock", handleLock(h.v))
	mux.HandleFunc("POST /vault/migrate", handleMigrate(h.v))

	if h.wa != nil {
		mux.HandleFunc("GET /vault/manage", handleManagePage())
		mux.HandleFunc("POST /vault/webauthn/register/begin", handleRegBegin(h.wa))
		mux.HandleFunc("POST /vault/webauthn/register/finish", handleRegFinish(h.wa))
		mux.HandleFunc("POST /vault/webauthn/login/begin", handleLoginBegin(h.wa))
		mux.HandleFunc("POST /vault/webauthn/login/finish", h.webauthnLimiter.Wrap(handleLoginFinish(h.wa)))
		mux.HandleFunc("GET /vault/webauthn/credentials", handleCredentials(h.v))
		mux.HandleFunc("POST /vault/webauthn/credentials/delete", handleDeleteCredential(h.v))
	}

	if h.queue != nil {
		RegisterApprovalHandlers(mux, h.queue, h.v)
	}
}

// RegisterHandlers is a convenience wrapper that creates handlers and mounts them.
// For mounting on multiple muxes, use NewVaultHandlers + Mount instead.
func RegisterHandlers(mux *http.ServeMux, v *Vault, wa *WebAuthnServer, queue *ApprovalQueue) {
	NewVaultHandlers(v, wa, queue).Mount(mux)
}

// --- Page handlers ---

func handleManagePage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		io.WriteString(w, managePageHTML)
	}
}

// --- Vault core handlers ---

func handleStatus(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, v.Status())
	}
}

func handleInit(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Passphrase string `json:"passphrase"`
		}
		if !readJSON(r, &req) || req.Passphrase == "" {
			writeError(w, http.StatusBadRequest, "passphrase required")
			return
		}

		if len(req.Passphrase) < 8 {
			writeError(w, http.StatusBadRequest, "passphrase must be at least 8 characters")
			return
		}

		if err := v.Init(req.Passphrase); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, v.Status())
	}
}

func handleUnlock(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Passphrase string `json:"passphrase"`
		}
		if !readJSON(r, &req) || req.Passphrase == "" {
			writeError(w, http.StatusBadRequest, "passphrase required")
			return
		}

		if err := v.UnlockWithPassphrase(req.Passphrase); err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, v.Status())
	}
}

func handleLock(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v.Lock()
		writeJSON(w, http.StatusOK, v.Status())
	}
}

func handleMigrate(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Secrets map[string]string `json:"secrets"`
		}
		if !readJSON(r, &req) {
			writeError(w, http.StatusBadRequest, "invalid request")
			return
		}

		if v.State() != StateUnlocked {
			writeError(w, http.StatusConflict, "vault must be unlocked to migrate secrets")
			return
		}

		migrated := 0
		for key, value := range req.Secrets {
			if err := v.SetSecret(key, value); err != nil {
				log.Printf("[vault] migrate %q: %v", key, err)
				continue
			}
			migrated++
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"migrated": migrated,
			"total":    len(req.Secrets),
		})
	}
}

// --- WebAuthn handlers ---

func handleRegBegin(wa *WebAuthnServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if wa.vault.State() != StateUnlocked {
			writeError(w, http.StatusForbidden, "vault must be unlocked to register credentials")
			return
		}
		options, err := wa.BeginRegistration()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(options)
	}
}

func handleRegFinish(wa *WebAuthnServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if wa.vault.State() != StateUnlocked {
			writeError(w, http.StatusForbidden, "vault must be unlocked to register credentials")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}

		// Extract credential name from header (body is the raw WebAuthn response).
		// Header avoids log leakage that query params would cause.
		name := r.Header.Get("X-Credential-Name")
		if name == "" {
			name = "unnamed"
		}

		cred, err := wa.FinishRegistration(body, name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "registered",
			"name":   cred.Name,
			"id":     cred.ID,
		})
	}
}

func handleLoginBegin(wa *WebAuthnServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		options, err := wa.BeginLogin()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(options)
	}
}

func handleLoginFinish(wa *WebAuthnServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}

		sessionToken, err := wa.FinishLogin(body)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "authenticated",
			"session_token": sessionToken,
			"vault":         wa.vault.Status(),
		})
	}
}

func handleCredentials(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v.State() != StateUnlocked {
			writeError(w, http.StatusForbidden, "vault must be unlocked")
			return
		}
		creds := v.Credentials()
		// Strip public key from response (not needed by frontend)
		type credInfo struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			AAGUID    string `json:"aaguid"`
			SignCount uint32 `json:"sign_count"`
			CreatedAt string `json:"created_at"`
		}
		var result []credInfo
		for _, c := range creds {
			result = append(result, credInfo{
				ID:        c.ID,
				Name:      c.Name,
				AAGUID:    c.AAGUID,
				SignCount: c.SignCount,
				CreatedAt: c.CreatedAt.Format("2006-01-02T15:04:05Z"),
			})
		}
		if result == nil {
			result = []credInfo{}
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func handleDeleteCredential(v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v.State() != StateUnlocked {
			writeError(w, http.StatusForbidden, "vault must be unlocked")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if !readJSON(r, &req) || req.ID == "" {
			writeError(w, http.StatusBadRequest, "credential id required")
			return
		}

		if err := v.RemoveCredential(req.ID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// --- HTTP helpers ---

func readJSON(r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return false
	}
	return json.Unmarshal(body, dst) == nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
