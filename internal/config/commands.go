package config

import (
	"fmt"
	"strings"
)

// CommandPattern is one entry of allow_commands or deny_commands on an ssh
// connection: a sequence of tokens that must match the start of a command's
// argument vector. "cat" matches `cat /etc/hosts`; "systemctl status" matches
// `systemctl status nginx` but not `systemctl restart nginx`. Tokens may
// contain the wildcards * and ? ("journalctl --vacuum*"). The program is
// matched exactly as given: "cat" does not cover `/bin/cat` — and it does not
// cover `/tmp/x/cat` either, which is the point. A connection that may run
// the full path lists it ("/bin/cat").
type CommandPattern struct {
	Tokens []string
	Raw    string
}

// ParseCommandPatterns reads pattern entries. Entries are split on
// whitespace; an empty entry is skipped, an entry that would match every
// command ("*") is an error — the lists exist to narrow, and a list that
// allows everything is better expressed by leaving it empty.
func ParseCommandPatterns(entries []string) ([]CommandPattern, error) {
	var out []CommandPattern
	for _, e := range entries {
		raw := strings.TrimSpace(e)
		if raw == "" {
			continue
		}
		tokens := strings.Fields(raw)
		if tokens[0] == "*" || tokens[0] == "**" {
			return nil, fmt.Errorf("entry %q would match every command — leave the list empty instead", raw)
		}
		out = append(out, CommandPattern{Tokens: tokens, Raw: raw})
	}
	return out, nil
}

// ValidateCommandPatterns is ParseCommandPatterns for callers that only need
// the verdict.
func ValidateCommandPatterns(entries []string) error {
	_, err := ParseCommandPatterns(entries)
	return err
}

// Matches reports whether the pattern covers argv: the pattern has no more
// tokens than argv and every token matches the argument in its position.
func (p CommandPattern) Matches(argv []string) bool {
	if len(p.Tokens) == 0 || len(p.Tokens) > len(argv) {
		return false
	}
	for i, tok := range p.Tokens {
		if !matchToken(tok, argv[i]) {
			return false
		}
	}
	return true
}

// matchToken compares one pattern token with one argument. Without wildcards
// it is a plain string comparison; with them, * matches any run of characters
// (including / — unlike path.Match) and ? exactly one.
func matchToken(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	return wildcardMatch(pattern, s)
}

func wildcardMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Collapse consecutive stars, then try every split point.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if wildcardMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if s == "" || pattern[0] != s[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return s == ""
}
