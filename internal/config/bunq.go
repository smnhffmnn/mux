package config

import (
	"fmt"
	"net/url"
	"strings"
)

// bunq API hosts. The sandbox is a separate installation with its own users
// and keys (https://doc.bunq.com/basics/environments); a key from one does
// not open the other.
const (
	BunqProductionURL = "https://api.bunq.com"
	BunqSandboxURL    = "https://public-api.sandbox.bunq.com"
)

// BunqSandbox reports whether a connection points at the sandbox host.
func BunqSandbox(c Connection) bool {
	return c.Type == TypeBunq && strings.EqualFold(strings.TrimRight(c.URL, "/"), BunqSandboxURL)
}

// ValidateBunq checks the fields of a bunq connection that are wrong before
// any request is made: the URL must be an https host without a path — the
// type appends /v1/… itself. ApplyConnectionDefaults fills the production
// host when the URL is empty.
func ValidateBunq(c Connection) error {
	if c.Type != TypeBunq {
		return nil // other types never read these fields
	}
	raw := strings.TrimSpace(c.URL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q: %w", c.URL, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("url must be an https host such as %s or %s, got %q", BunqProductionURL, BunqSandboxURL, c.URL)
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("url carries a path or query (%q) — give the host only, the type appends /v1 itself", c.URL)
	}
	return nil
}
