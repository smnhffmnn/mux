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

// ValidateNylas checks the fields of a nylas connection that are wrong before
// any request is made: the region must exist, and the grant must be
// addressable — by id, or by the mailbox address in user, which mux resolves
// through the grants API on first use.
func ValidateNylas(c Connection) error {
	if c.Type != TypeNylas {
		return nil // other types never read these fields
	}
	if !ValidNylasRegion(c.Region) {
		return fmt.Errorf("region %q is not a Nylas region (%s or %s)", c.Region, NylasRegionUS, NylasRegionEU)
	}
	if strings.TrimSpace(c.GrantID) == "" && strings.TrimSpace(c.User) == "" {
		return fmt.Errorf("needs grant_id, or user set to the mailbox address so the grant can be looked up")
	}
	if u := strings.TrimSpace(c.User); u != "" && !isMailAddress(u) {
		return fmt.Errorf("user must be the mailbox address (local@domain), got %q", c.User)
	}
	return nil
}
