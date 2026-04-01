package vault

import (
	"html/template"
	"log"
	"net/http"
	"strings"
)

// RegisterApprovalHandlers mounts approval-related HTTP endpoints.
func RegisterApprovalHandlers(mux *http.ServeMux, queue *ApprovalQueue, v *Vault) {
	mux.HandleFunc("POST /vault/approval", handleCreateApproval(queue, v))
	mux.HandleFunc("GET /vault/approval/{id}", handleGetApproval(queue))
	mux.HandleFunc("POST /vault/approval/{id}/grant", handleGrantApproval(queue, v))
	mux.HandleFunc("POST /vault/approval/{id}/deny", handleDenyApproval(queue, v))
	mux.HandleFunc("GET /vault/approve/{id}", handleApprovePage())
	mux.HandleFunc("GET /vault/approvals", handleListApprovals(queue))
}

func handleCreateApproval(queue *ApprovalQueue, v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Creating approvals requires the vault to be unlocked (prevents notification spam)
		if v != nil && v.State() != StateUnlocked {
			writeError(w, http.StatusForbidden, "vault must be unlocked to create approvals")
			return
		}

		var req struct {
			Action    string `json:"action"`
			Context   string `json:"context"`
			Requester string `json:"requester"`
		}
		if !readJSON(r, &req) || req.Action == "" {
			writeError(w, http.StatusBadRequest, "action required")
			return
		}

		approval, err := queue.Create(req.Action, req.Context, req.Requester)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, approval)
	}
}

func handleGetApproval(queue *ApprovalQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "approval id required")
			return
		}

		approval, err := queue.Get(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, approval)
	}
}

func handleGrantApproval(queue *ApprovalQueue, v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "approval id required")
			return
		}

		// Require session token from WebAuthn login (prevents grant without biometric auth).
		// Token is only accepted in the JSON body — never in query params (which leak in logs).
		var req struct {
			SessionToken string `json:"session_token"`
		}
		readJSON(r, &req)
		if req.SessionToken == "" || !v.ValidateSessionToken(req.SessionToken) {
			writeError(w, http.StatusUnauthorized, "valid session_token required — complete WebAuthn login first")
			return
		}

		if err := queue.Grant(id); err != nil {
			status := http.StatusConflict
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "granted"})
	}
}

func handleDenyApproval(queue *ApprovalQueue, v *Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "approval id required")
			return
		}

		// Require vault to be unlocked (prevents unauthenticated denial-of-service)
		if v != nil && v.State() != StateUnlocked {
			writeError(w, http.StatusUnauthorized, "vault must be unlocked to deny approvals")
			return
		}

		if err := queue.Deny(id); err != nil {
			status := http.StatusConflict
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
	}
}

func handleApprovePage() http.HandlerFunc {
	// Parse template once at registration time
	tmpl := template.Must(template.New("approve").Parse(approvalPageHTML))

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		if err := tmpl.Execute(w, nil); err != nil {
			log.Printf("[vault] approval page template error: %v", err)
		}
	}
}

func handleListApprovals(queue *ApprovalQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pending := queue.Pending()
		if pending == nil {
			pending = []ApprovalRequest{}
		}
		writeJSON(w, http.StatusOK, pending)
	}
}
