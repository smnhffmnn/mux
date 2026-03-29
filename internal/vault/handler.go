package vault

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

// RegisterHandlers mounts all vault HTTP endpoints on the given mux.
// The approval queue is optional (nil = no approval endpoints).
func RegisterHandlers(mux *http.ServeMux, v *Vault, wa *WebAuthnServer, queue *ApprovalQueue) {
	// Separate rate limiters for passphrase and WebAuthn auth (5 attempts per minute each)
	passphraseLimiter := NewRateLimiter(5, 1*time.Minute)
	webauthnLimiter := NewRateLimiter(5, 1*time.Minute)

	mux.HandleFunc("GET /vault/status", handleStatus(v))
	mux.HandleFunc("POST /vault/init", handleInit(v))
	mux.HandleFunc("POST /vault/unlock", passphraseLimiter.Wrap(handleUnlock(v)))
	mux.HandleFunc("POST /vault/lock", handleLock(v))
	mux.HandleFunc("POST /vault/migrate", handleMigrate(v))

	if wa != nil {
		mux.HandleFunc("POST /vault/webauthn/register/begin", handleRegBegin(wa))
		mux.HandleFunc("POST /vault/webauthn/register/finish", handleRegFinish(wa))
		mux.HandleFunc("POST /vault/webauthn/login/begin", handleLoginBegin(wa))
		mux.HandleFunc("POST /vault/webauthn/login/finish", webauthnLimiter.Wrap(handleLoginFinish(wa)))
		mux.HandleFunc("GET /vault/webauthn/credentials", handleCredentials(v))
		mux.HandleFunc("POST /vault/webauthn/credentials/delete", handleDeleteCredential(v))
	}

	if queue != nil {
		RegisterApprovalHandlers(mux, queue, v)
		log.Println("[vault] Approval endpoints registered on /vault/approval/*")
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
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}

		// Extract credential name from query param (body is the raw WebAuthn response)
		name := r.URL.Query().Get("name")
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
