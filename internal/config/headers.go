package config

import (
	"fmt"
	"sort"
	"strings"
)

// ValidHeaderName reports whether s is a valid HTTP header field name
// (RFC 7230 token).
func ValidHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c >= 0x7f || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", c) {
			return false
		}
	}
	return true
}

// validHeaderValue reports whether s is safe as an HTTP header field value:
// no control characters (in particular no CR/LF, which would allow header
// injection). Horizontal tab is permitted per RFC 7230.
func validHeaderValue(s string) bool {
	for _, c := range s {
		if (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// ParseHeaderLines parses newline-separated "Name: Value" pairs into a header
// map. Blank lines are skipped. Returns an error on lines without a colon,
// invalid header names, or unsafe values. An empty input yields a nil map.
func ParseHeaderLines(s string) (map[string]string, error) {
	var headers map[string]string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("invalid header line %q: expected \"Name: Value\"", line)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ValidHeaderName(name) {
			return nil, fmt.Errorf("invalid header name %q", name)
		}
		if !validHeaderValue(value) {
			return nil, fmt.Errorf("invalid value for header %q: control characters are not allowed", name)
		}
		if headers == nil {
			headers = make(map[string]string)
		}
		headers[name] = value
	}
	return headers, nil
}

// FormatHeaderLines renders a header map as newline-separated "Name: Value"
// lines, sorted by name for deterministic output. The inverse of
// ParseHeaderLines.
func FormatHeaderLines(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(headers[name])
	}
	return b.String()
}
