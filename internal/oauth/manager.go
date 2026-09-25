// Package oauth implements the browser-based OAuth 2.0 authorization flow
// mux uses to authorize proxy connections against upstream MCP servers:
// metadata discovery, PKCE, dynamic client registration, the local callback
// endpoint, and the pending-flow state in between.
//
// The package is deliberately UI-agnostic and free of build tags: the Wails
// desktop app and the headless HTTP mode mount the same routes and share the
// same flow state. Keep it that way — code here must not depend on Wails,
// and notray builds must keep working (before the extraction the flow lived
// in the tag-guarded app.go, which left notray builds without OAuth at all).
package oauth

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/proxy"
)

// Manager runs OAuth authorization flows and tracks their pending state
// between the authorization redirect and the provider's callback.
type Manager struct {
	cfg  *config.Config
	port int

	// onAuthorized is called synchronously after a successful token
	// exchange, with the connection name. Long-running work (mounting the
	// proxy) belongs in a goroutine inside the callback — the provider's
	// browser redirect is waiting on the HTTP response.
	onAuthorized func(service string)

	mu      sync.Mutex
	pending map[string]*flow
}

// flow is one authorization attempt, keyed by its state parameter.
type flow struct {
	service      string
	handler      *transport.OAuthHandler
	codeVerifier string
	tokenStore   *config.KeychainTokenStore
}

// NewManager creates a Manager. onAuthorized may be nil.
func NewManager(cfg *config.Config, port int, onAuthorized func(service string)) *Manager {
	return &Manager{
		cfg:          cfg,
		port:         port,
		onAuthorized: onAuthorized,
		pending:      make(map[string]*flow),
	}
}

// Start begins an OAuth flow for the named proxy connection and returns the
// provider's authorization URL for the user's browser.
func (m *Manager) Start(name string) (string, error) {
	conn := m.cfg.FindAnyConnection(name)
	if conn == nil || !conn.OAuth {
		return "", fmt.Errorf("OAuth not supported for: %s", name)
	}

	mcpURL := conn.URL
	if mcpURL == "" {
		return "", fmt.Errorf("URL not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	metadataURL, err := DiscoverMetadata(ctx, mcpURL)
	if err != nil {
		return "", fmt.Errorf("OAuth discovery failed: %w", err)
	}
	log.Printf("[oauth] Discovered metadata URL for %s: %s", conn.Name, metadataURL)

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth/callback", m.port)
	tokenStore := config.NewKeychainTokenStore(conn.Name)
	adapter := proxy.NewKeychainTokenAdapter(tokenStore)
	clientID, clientSecret := config.LoadOAuthClientID(conn.Name)

	var scopes []string
	if conn.Scopes != "" {
		for _, s := range strings.Split(conn.Scopes, " ") {
			s = strings.TrimSpace(s)
			if s != "" {
				scopes = append(scopes, s)
			}
		}
	}

	oauthCfg := transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		Scopes:                scopes,
		TokenStore:            adapter,
		PKCEEnabled:           true,
		AuthServerMetadataURL: metadataURL,
	}

	oauthHandler := transport.NewOAuthHandler(oauthCfg)
	oauthHandler.SetBaseURL(mcpURL)

	if clientID == "" {
		if err := oauthHandler.RegisterClient(ctx, "mux"); err != nil {
			return "", fmt.Errorf("client registration failed: %w", err)
		}
		if err := config.SaveOAuthClient(conn.Name, oauthHandler.GetClientID(), oauthHandler.GetClientSecret()); err != nil {
			log.Printf("[oauth] Warning: could not save OAuth client to keychain: %v", err)
		}
		log.Printf("[oauth] Registered client for %s: %s", conn.Name, oauthHandler.GetClientID())
	}

	codeVerifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("failed to generate code verifier")
	}
	codeChallenge := transport.GenerateCodeChallenge(codeVerifier)

	state, err := transport.GenerateState()
	if err != nil {
		return "", fmt.Errorf("failed to generate state")
	}

	authURL, err := oauthHandler.GetAuthorizationURL(ctx, state, codeChallenge)
	if err != nil {
		return "", fmt.Errorf("failed to get auth URL: %w", err)
	}

	m.mu.Lock()
	m.pending[state] = &flow{
		service:      conn.Name,
		handler:      oauthHandler,
		codeVerifier: codeVerifier,
		tokenStore:   tokenStore,
	}
	m.mu.Unlock()

	log.Printf("[oauth] Started OAuth flow for %s", conn.Name)
	return authURL, nil
}

// CallbackHandler serves /oauth/callback: it matches the state parameter to
// a pending flow, exchanges the code for tokens, and notifies onAuthorized.
func (m *Manager) CallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" || state == "" {
			errorMsg := r.URL.Query().Get("error_description")
			if errorMsg == "" {
				errorMsg = r.URL.Query().Get("error")
			}
			if errorMsg == "" {
				errorMsg = "Missing code or state parameter"
			}
			writeCallbackPage(w, "Error", "#f87171", errorMsg)
			return
		}

		m.mu.Lock()
		pending, ok := m.pending[state]
		if ok {
			delete(m.pending, state)
		}
		m.mu.Unlock()

		if !ok {
			writeCallbackPage(w, "Error", "#f87171", "Unknown or expired OAuth state")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := pending.handler.ProcessAuthorizationResponse(ctx, code, state, pending.codeVerifier); err != nil {
			writeCallbackPage(w, "Error", "#f87171", fmt.Sprintf("Token exchange failed: %v", err))
			return
		}

		log.Printf("[oauth] Successfully authorized %s", pending.service)

		if m.onAuthorized != nil {
			m.onAuthorized(pending.service)
		}

		writeCallbackPage(w, "Success", "#34d399", fmt.Sprintf("Successfully authorized %s! You can close this tab.", pending.service))
	}
}

// StartHandler serves /oauth/start?connection=<name>: a browser-based entry
// point that redirects to the provider's authorization page. Headless mode
// has no UI to trigger flows, so this endpoint is the entry point there; the
// desktop app triggers flows through its own bindings but mounts it too.
func (m *Manager) StartHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("connection")
		if name == "" {
			http.Error(w, "missing ?connection= parameter", http.StatusBadRequest)
			return
		}
		authURL, err := m.Start(name)
		if err != nil {
			log.Printf("[oauth] Start error for %q: %v", name, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

// Routes mounts the OAuth endpoints (/oauth/start, /oauth/callback).
func (m *Manager) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/oauth/callback", m.CallbackHandler())
	mux.HandleFunc("/oauth/start", m.StartHandler())
	log.Printf("[oauth] OAuth routes mounted (/oauth/start, /oauth/callback)")
}

func writeCallbackPage(w http.ResponseWriter, status, color, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, callbackHTML(status, color, message))
}

func callbackHTML(status, color, message string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>mux - OAuth %s</title>
<style>body{background:#0f1117;color:#e2e4e9;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{text-align:center;padding:2rem;border-radius:12px;border:1px solid #2a2d35}
h2{color:%s}</style></head>
<body><div class="card"><h2>%s</h2><p>%s</p></div></body></html>`,
		html.EscapeString(status), color, html.EscapeString(status), html.EscapeString(message))
}
