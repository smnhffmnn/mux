package config

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// HetznerCloudDefaultURL is the Cloud API host. A field so a test server
// can stand in; a real project is always behind this host.
const HetznerCloudDefaultURL = "https://api.hetzner.cloud"

// HetznerCloudFields are the config keys that only the hetzner-cloud type
// reads. Set on another type they would be stored and never used, which
// hides a typo in the type (same rule as SupabaseFields).
var HetznerCloudFields = []string{"allow_lifecycle", "protected_resources"}

// HetznerCloudKinds are the resource kinds the hetzner-cloud type knows,
// as the tools name them (the API's collection is the plural). A
// protected_resources entry may name one of them before the colon; the
// tools package keeps the registry and a test holds both lists together.
var HetznerCloudKinds = []string{
	"server", "image", "iso", "volume", "network", "firewall", "floating_ip", "primary_ip",
	"load_balancer", "load_balancer_type", "certificate", "placement_group", "ssh_key",
	"location", "server_type", "zone",
}

// HetznerCloudBaseURL normalizes the configured API URL: scheme and host, no
// path, no trailing slash. Empty means the default host. Plain http is
// allowed for loopback hosts only (tests) — the token would travel in clear
// text otherwise.
func HetznerCloudBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return HetznerCloudDefaultURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("url %q must be http(s)://host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("url %q must be the bare API host, without query, fragment or credentials", raw)
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return "", fmt.Errorf("url %q must be the bare API host — the type adds /v1/… itself", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("url %q: plain http is allowed for loopback hosts only — the API token would travel in clear text", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ResourceGuard names a Hetzner Cloud resource the connection never changes,
// acts on or deletes — read from protected_resources. Kind "" means every
// kind; Pattern is matched against the resource's name with path.Match
// semantics (* and ?), case-insensitively.
type ResourceGuard struct {
	Kind    string
	Pattern string
}

// ParseResourceGuards reads "name", "kind:name" and pattern entries. "*" or
// an empty kind stands for every kind. A kind that is not one of
// HetznerCloudKinds, an entry without a name and an unparsable pattern are
// errors, so a typo is caught where the list is entered (desktop form,
// connection_add) instead of silently guarding nothing.
func ParseResourceGuards(entries []string) ([]ResourceGuard, error) {
	var out []ResourceGuard
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		kind, pattern, found := strings.Cut(e, ":")
		if !found {
			pattern, kind = kind, ""
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		pattern = strings.TrimSpace(pattern)
		if kind == "*" {
			kind = ""
		}
		if pattern == "" {
			return nil, fmt.Errorf("entry %q names no resource (expected \"name\", \"kind:name\" or a pattern with *)", e)
		}
		if kind != "" && !hetznerKind(kind) {
			return nil, fmt.Errorf("entry %q: unknown kind %q (one of %s)", e, kind, strings.Join(HetznerCloudKinds, ", "))
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("entry %q: bad pattern: %v", e, err)
		}
		out = append(out, ResourceGuard{Kind: kind, Pattern: strings.ToLower(pattern)})
	}
	return out, nil
}

// ValidateResourceGuards is ParseResourceGuards for callers that only need
// the verdict.
func ValidateResourceGuards(entries []string) error {
	_, err := ParseResourceGuards(entries)
	return err
}

func hetznerKind(kind string) bool {
	for _, k := range HetznerCloudKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Matches reports whether the guard covers a resource of this kind with
// this name.
func (g ResourceGuard) Matches(kind, name string) bool {
	if g.Kind != "" && !strings.EqualFold(g.Kind, kind) {
		return false
	}
	ok, err := path.Match(g.Pattern, strings.ToLower(strings.TrimSpace(name)))
	return err == nil && ok
}

// ValidateHetznerCloud checks the fields of a hetzner-cloud connection that
// are wrong before any request is made: the URL must be a bare host, the
// protected_resources entries must parse, and read_only together with
// allow_lifecycle is a contradiction (same rule as nylas with allow_send).
func ValidateHetznerCloud(c Connection) error {
	if c.Type != TypeHetznerCloud {
		return nil // other types never read these fields
	}
	if _, err := HetznerCloudBaseURL(c.URL); err != nil {
		return err
	}
	if err := ValidateResourceGuards(c.ProtectedResources); err != nil {
		return fmt.Errorf("protected_resources: %w", err)
	}
	if c.ReadOnly && c.AllowLifecycle {
		return fmt.Errorf("read_only and allow_lifecycle together make no sense — a read-only connection registers no write tool; drop one of them")
	}
	return nil
}
