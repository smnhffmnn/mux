package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// SupabaseDefaultURL is the Management API host. It is a field so a test
// server or a proxy can stand in; a real project is always behind this host.
const SupabaseDefaultURL = "https://api.supabase.com"

// SupabaseFields are the config keys that only the supabase type reads. Set
// on another type they would be stored and never used, which hides a typo in
// the type (same rule as IMAPOnlyFields on the imap type).
var SupabaseFields = []string{"project_ref"}

// supabaseProjectRef is the form of a project reference: twenty lowercase
// letters, as the Management API's OpenAPI document declares it.
var supabaseProjectRef = regexp.MustCompile(`^[a-z]{20}$`)

// ValidSupabaseProjectRef reports whether ref has the form of a project
// reference.
func ValidSupabaseProjectRef(ref string) bool {
	return supabaseProjectRef.MatchString(ref)
}

// SupabaseBaseURL normalizes the configured API URL: scheme and host, no
// path, no trailing slash. Empty means the default host.
func SupabaseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SupabaseDefaultURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("url %q must be http(s)://host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("url %q must be the bare API host, without query, fragment or credentials", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	if path != "" {
		return "", fmt.Errorf("url %q must be the bare API host — the type adds /v1/… itself", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ValidateSupabase checks the fields of a supabase connection that are wrong
// before any request is made: the project reference must have the documented
// form and the URL must be a bare host.
func ValidateSupabase(c Connection) error {
	if c.Type != TypeSupabase {
		return nil // other types never read these fields
	}
	ref := strings.TrimSpace(c.ProjectRef)
	if ref == "" {
		return fmt.Errorf("needs project_ref, the project's reference (20 lowercase letters, the id in the dashboard URL)")
	}
	if !ValidSupabaseProjectRef(ref) {
		return fmt.Errorf("project_ref %q is not a project reference (20 lowercase letters)", c.ProjectRef)
	}
	if _, err := SupabaseBaseURL(c.URL); err != nil {
		return err
	}
	return nil
}
