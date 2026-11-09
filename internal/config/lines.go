package config

import (
	"fmt"
	"strings"
)

// ParseLines splits a newline-separated UI value into trimmed, non-empty
// entries. It is the counterpart of FormatLines for list-valued connection
// fields that the desktop form renders as a textarea (protected_fields,
// set_once_fields). An input without entries yields nil, so an emptied
// textarea clears the field instead of storing an empty list.
func ParseLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// FormatLines renders list entries one per line for the desktop form.
func FormatLines(entries []string) string {
	return strings.Join(entries, "\n")
}

// ParseLinesOrList reads a list argument that a client may send either as
// newline-separated text (the desktop form, TOML strings) or as a JSON array
// of strings (connection_add from an agent, provisioning). Any other type is
// an error — a list that is silently dropped would guard nothing.
func ParseLinesOrList(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return ParseLines(x), nil
	case []string:
		return ParseLines(strings.Join(x, "\n")), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("list entry %v is not a string", e)
			}
			parts = append(parts, s)
		}
		return ParseLines(strings.Join(parts, "\n")), nil
	default:
		return nil, fmt.Errorf("expected a string with one entry per line or a list of strings, got %T", v)
	}
}
