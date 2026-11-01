package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// BlobsConfig is the [blobs] table: the temporary file store mux serves on
// its loopback listener (POST/GET /blob), through which files move between
// an agent and mux when the two do not share a disk. Empty fields take the
// store's defaults (an hour, 1 GiB in total, 256 MiB per blob).
type BlobsConfig struct {
	TTL      string `toml:"ttl,omitempty" json:"ttl,omitempty"`            // lifetime of a blob, e.g. "1h"
	MaxTotal string `toml:"max_total,omitempty" json:"maxTotal,omitempty"` // cap for all blobs on disk, e.g. "1GiB"
	MaxBlob  string `toml:"max_blob,omitempty" json:"maxBlob,omitempty"`   // cap for one blob, e.g. "256MiB"
}

// EffectiveTTL parses ttl; zero means "use the default".
func (b BlobsConfig) EffectiveTTL() (time.Duration, error) {
	if strings.TrimSpace(b.TTL) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(b.TTL))
	if err != nil {
		return 0, fmt.Errorf("[blobs] ttl %q: %w", b.TTL, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("[blobs] ttl must be positive, got %q", b.TTL)
	}
	return d, nil
}

// EffectiveMaxTotal parses max_total; zero means "use the default".
func (b BlobsConfig) EffectiveMaxTotal() (int64, error) {
	n, err := ParseSize(b.MaxTotal)
	if err != nil {
		return 0, fmt.Errorf("[blobs] max_total: %w", err)
	}
	return n, nil
}

// EffectiveMaxBlob parses max_blob; zero means "use the default".
func (b BlobsConfig) EffectiveMaxBlob() (int64, error) {
	n, err := ParseSize(b.MaxBlob)
	if err != nil {
		return 0, fmt.Errorf("[blobs] max_blob: %w", err)
	}
	return n, nil
}

// ParseSize reads a byte count with an optional unit: "512", "64KB", "1MiB",
// "2G". Units are binary (K = 1024) whether or not they carry the "i" — a
// disk cap written as "1GB" means the gibibyte the operator has in mind. An
// empty string is zero, not an error.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	if num == "" {
		return 0, fmt.Errorf("size %q has no number", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "B"), "I")
	var mult float64
	switch unit {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size %q: unknown unit", s)
	}
	n := f * mult
	if n < 0 || n > float64(1<<62) {
		return 0, fmt.Errorf("size %q is out of range", s)
	}
	return int64(n), nil
}

// StateDir returns the directory for data mux keeps between requests but not
// between installations: the log file and the blob store.
// Windows: <Dir()>\state
// Otherwise: $XDG_STATE_HOME/mux if set to an absolute path, ~/.local/state/mux
// as fallback.
func StateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(Dir(), "state")
	}
	return xdgStateDir()
}

// xdgStateDir is the XDG state directory for mux, shared by LogDir and
// StateDir on non-Windows systems.
func xdgStateDir() string {
	// The spec requires ignoring relative values.
	if xdg := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, ServiceName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", ServiceName)
}
