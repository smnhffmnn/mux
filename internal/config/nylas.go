package config

import (
	"fmt"
	"strings"
)

// Nylas data-residency regions. The region of a connection is the region of
// the Nylas application that owns the grant; a key from one region does not
// open a grant in the other.
const (
	NylasRegionUS = "us"
	NylasRegionEU = "eu"
)

// NylasFields are the config keys that only the nylas type reads. Set on
// another type they would be stored and never used, which hides a typo in the
// type (same rule as IMAPOnlyFields).
var NylasFields = []string{"region", "grant_id"}

// ValidNylasRegion reports whether region is one Nylas serves. The empty
// string is valid here because ApplyConnectionDefaults fills it.
func ValidNylasRegion(region string) bool {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "", NylasRegionUS, NylasRegionEU:
		return true
	}
	return false
}

// NylasBaseURL returns the REST API host for a region. The contract is the
// versioned v3 API (https://developer.nylas.com/openapi.json), not Nylas'
// hosted MCP server — that one is unversioned and reachable through the proxy
// type instead.
func NylasBaseURL(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = NylasRegionUS
	}
	return "https://api." + region + ".nylas.com"
}

// NylasApplicationMode reports whether a nylas connection serves every grant
// of its application rather than one mailbox: neither grant_id nor user names
// a mailbox, so the key must be an application key (or an IAM key bound to
// the application), and the mail tools take the mailbox as an argument.
func NylasApplicationMode(c Connection) bool {
	return c.Type == TypeNylas && strings.TrimSpace(c.GrantID) == "" && strings.TrimSpace(c.User) == ""
}

// ValidateNylas checks the fields of a nylas connection that are wrong before
// any request is made: the region must exist, user — when set — must be a
// mailbox address, and read_only and allow_send must not both be set. Which
// mailbox the connection serves is decided by grant_id and user: an id, an
// address mux resolves through the grants API on first use, or neither —
// then the key serves every grant of the application (NylasApplicationMode).
func ValidateNylas(c Connection) error {
	if c.Type != TypeNylas {
		return nil // other types never read these fields
	}
	if !ValidNylasRegion(c.Region) {
		return fmt.Errorf("region %q is not a Nylas region (%s or %s)", c.Region, NylasRegionUS, NylasRegionEU)
	}
	if u := strings.TrimSpace(c.User); u != "" && !isMailAddress(u) {
		return fmt.Errorf("user must be the mailbox address (local@domain), got %q", c.User)
	}
	if c.ReadOnly && c.AllowSend {
		// Refused rather than resolved one way: a read-only connection never
		// sends, and a silent winner would hide a misconfiguration.
		return fmt.Errorf("read_only and allow_send contradict each other — a read-only connection never sends; drop one of them")
	}
	return nil
}
