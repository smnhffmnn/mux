package config

import "strings"

// maxToolNameLen is the length limit of the strictest known MCP client
// (Claude.ai chat validates tool names against ^[a-zA-Z0-9_-]{1,64}$).
const maxToolNameLen = 64

// SanitizeToolName makes s safe as an MCP tool name: every character outside
// [a-zA-Z0-9_-] becomes '_', and the result is truncated to 64 characters.
// Connection names are human-readable display names (spaces, colons, umlauts
// are all legal there); tool names built from them must satisfy the strictest
// client pattern or the client rejects the entire tool list. Idempotent —
// sanitizing an already-sanitized name is a no-op.
//
// Note: truncation can in theory collide two very long names that share their
// first 64 characters; acceptable, since tool names derive from connection
// names that users keep short and distinct.
func SanitizeToolName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > maxToolNameLen {
		name = name[:maxToolNameLen]
	}
	return name
}
