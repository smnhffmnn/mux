package tools

import (
	"fmt"
	"strings"

	"github.com/smnhffmnn/mux/internal/config"
)

// Command policy for the ssh connection type.
//
// An SSH "exec" request carries one string, and the remote login shell
// interprets it — there is no argv on the wire. So "arguments as a list, no
// shell" is implemented on this side: every argument is quoted for a POSIX
// shell and joined, which makes the remote shell see exactly the list the
// caller passed. A policy entry like "systemctl status" is then checked
// against that list, not against a string an agent could have assembled.
//
// What the policy is and is not: allow_commands and deny_commands express the
// operator's intent about which programs a connection may start. They are
// enforced in code for every client. They are not a sandbox — `cat` as root
// reads every file, `tail -f` runs until the timeout — and the docs say so.

// defaultReadOnlyCommands is the allow list a read-only connection runs when
// allow_commands is empty: programs that observe and do not change. Not on
// it, deliberately: anything that runs another program (env, nice, timeout,
// xargs, find -exec) — such a program turns the list into "anything" — and
// hostname, which sets the name when given one (uname -n reads it).
var defaultReadOnlyCommands = []string{
	"ls", "cat", "head", "tail", "grep", "stat", "wc", "file",
	"df", "du", "free", "uptime", "ps", "uname", "id", "whoami", "date",
	"systemctl status", "systemctl is-active", "systemctl list-units",
	"journalctl",
	"docker ps", "docker logs", "docker inspect",
	"podman ps", "podman logs", "podman inspect",
}

// defaultReadOnlyDenied closes the gaps the allow list leaves: journalctl is
// a reader except for the flags that delete or rotate journal files, date is
// a reader except when it sets the clock.
var defaultReadOnlyDenied = []string{
	"journalctl --vacuum*",
	"journalctl --rotate",
	"journalctl --flush",
	"journalctl --relinquish-var",
	"date -s*",
	"date --set*",
}

// DefaultReadOnlyCommands returns the built-in allow list (a copy), for the
// type's dry_run output.
func DefaultReadOnlyCommands() []string {
	return append([]string(nil), defaultReadOnlyCommands...)
}

// shellAllowed says whether shell=true may run on a connection with this
// policy: only where no list applies at all. A shell line cannot be checked
// against allow or deny entries, so any entry — set by the operator or
// implied by read_only — rules it out.
func (p *commandPolicy) shellAllowed() string {
	switch {
	case p.readOnly:
		return "refused: shell=true is not available on a read-only connection"
	case len(p.allow) > 0:
		return "refused: shell=true bypasses allow_commands, which this connection sets"
	case len(p.deny) > 0:
		return "refused: shell=true bypasses deny_commands, which this connection sets"
	}
	return ""
}

// commandPolicy decides whether a command may run on a connection.
type commandPolicy struct {
	readOnly bool
	allow    []config.CommandPattern // empty + readOnly → defaults
	deny     []config.CommandPattern // always applied
	// usingDefaults is true when readOnly and no allow_commands were set.
	usingDefaults bool
}

func newCommandPolicy(conn config.Connection) (*commandPolicy, error) {
	allow, err := config.ParseCommandPatterns(conn.AllowCommands)
	if err != nil {
		return nil, fmt.Errorf("allow_commands: %w", err)
	}
	deny, err := config.ParseCommandPatterns(conn.DenyCommands)
	if err != nil {
		return nil, fmt.Errorf("deny_commands: %w", err)
	}
	p := &commandPolicy{readOnly: conn.ReadOnly, allow: allow, deny: deny}
	if conn.ReadOnly && len(allow) == 0 {
		p.allow, _ = config.ParseCommandPatterns(defaultReadOnlyCommands)
		defaults, _ := config.ParseCommandPatterns(defaultReadOnlyDenied)
		p.deny = append(defaults, p.deny...)
		p.usingDefaults = true
	}
	return p, nil
}

// decide returns "" when argv may run, otherwise the reason it may not.
// Deny entries win over allow entries; without an allow list and without
// read_only, anything not denied runs.
func (p *commandPolicy) decide(argv []string) string {
	if len(argv) == 0 {
		return "no command given"
	}
	for _, d := range p.deny {
		if d.Matches(argv) {
			return fmt.Sprintf("refused: %q matches deny_commands entry %q", argv[0], d.Raw)
		}
	}
	if len(p.allow) == 0 {
		return ""
	}
	for _, a := range p.allow {
		if a.Matches(argv) {
			return ""
		}
	}
	list := "allow_commands"
	if p.usingDefaults {
		list = "the built-in read-only list (" + strings.Join(defaultReadOnlyCommands, ", ") + ")"
	}
	return fmt.Sprintf("refused: %q is not in %s", strings.Join(argv, " "), list)
}

// describe says which lists apply, for dry_run and the test button.
func (p *commandPolicy) describe() string {
	switch {
	case p.usingDefaults:
		return "read-only: built-in allow list, deny " + strings.Join(defaultReadOnlyDenied, ", ")
	case len(p.allow) > 0 && len(p.deny) > 0:
		return fmt.Sprintf("%d allow_commands, %d deny_commands", len(p.allow), len(p.deny))
	case len(p.allow) > 0:
		return fmt.Sprintf("%d allow_commands", len(p.allow))
	case len(p.deny) > 0:
		return fmt.Sprintf("any command except %d deny_commands", len(p.deny))
	default:
		return "any command"
	}
}

// shellQuote renders argv as one POSIX shell command line in which every
// argument is a single-quoted literal. The remote shell splits it back into
// exactly argv: no globbing, no variable expansion, no operators. A single
// quote inside an argument becomes '\” (close, escaped quote, reopen).
// Empty arguments survive as ”.
func shellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(a)
	}
	return strings.Join(parts, " ")
}

func quoteArg(a string) string {
	if a == "" {
		return "''"
	}
	if isPlainWord(a) {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// isPlainWord reports whether a needs no quoting: letters, digits and the
// punctuation that no POSIX shell treats specially. Keeping plain words bare
// makes the logged command readable (`systemctl status nginx`, not
// `'systemctl' 'status' 'nginx'`). '=' is not plain: a bare leading NAME=value
// word is a variable assignment to the shell, which would move the real
// program to the second word and past the policy check.
func isPlainWord(a string) bool {
	for _, r := range a {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == '@', r == '%', r == '+', r == ',':
		default:
			return false
		}
	}
	return true
}
