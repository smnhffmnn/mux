package config

import "strings"

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
