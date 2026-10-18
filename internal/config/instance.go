package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Instance identifies one mux installation towards a provisioning server.
//
// Provisioning authenticates with a per-user token, so without more the
// server cannot tell two installations of the same user apart and hands both
// the same tunnel credential — and a WireGuard server keeps one endpoint per
// key, so the two then steal each other's return path. The instance ID is the
// missing dimension: a stable name per installation that the server can key
// its own tunnel credentials on (one peer per instance). It is only a name,
// never key material.
type Instance struct {
	// ID is stable for the lifetime of the installation. Empty means the
	// instance has no identity and the server serves it like any other
	// client of the user.
	ID string
	// Hostname is informational (what the server shows an admin) and may be
	// empty.
	Hostname string
}

const (
	// InstanceIDEnv overrides the persisted instance ID — for containers,
	// whose config directory does not survive a rebuild.
	InstanceIDEnv = "MUX_INSTANCE_ID"

	// instanceFile holds the generated ID inside the config directory. It is
	// deliberately not part of config.toml: the file is written once and
	// never rewritten by the config writers.
	instanceFile = "instance"

	instanceIDMaxLen   = 64
	instanceHostMaxLen = 40
)

// instanceIDPattern mirrors what provisioning servers accept as a device
// slug: lowercase alphanumeric segments joined by single "-" or "_".
var instanceIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[_-][a-z0-9]+)*$`)

var (
	instanceMu  sync.Mutex
	instanceVal Instance
)

// InstanceIdentity returns this installation's identity: MUX_INSTANCE_ID
// wins, otherwise the `instance` file in the config directory, which is
// created on first use. An ID that cannot be persisted is not reported — a
// fresh ID on every start would register a new instance on every start.
//
// A resolved identity is cached for the process. A failed resolution is not:
// the next fetch (vault-unlock retry, UI sync) tries again, so a config
// directory that was unwritable at startup is picked up once it is writable.
func InstanceIdentity() Instance {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	if instanceVal.ID == "" {
		instanceVal = loadInstance(Dir())
	}
	return instanceVal
}

// ValidInstanceID reports whether id is acceptable as an instance ID.
func ValidInstanceID(id string) bool {
	return id != "" && len(id) <= instanceIDMaxLen && instanceIDPattern.MatchString(id)
}

// loadInstance resolves the identity against dir without the process-wide
// cache.
func loadInstance(dir string) Instance {
	host := shortHostname()

	if env := strings.TrimSpace(os.Getenv(InstanceIDEnv)); env != "" {
		id := strings.ToLower(env)
		if !ValidInstanceID(id) {
			log.Printf("[instance] %s=%q is not a valid instance id (lowercase alphanumeric segments joined by - or _, at most %d chars) — provisioning without identity",
				InstanceIDEnv, env, instanceIDMaxLen)
			return Instance{Hostname: host}
		}
		return Instance{ID: id, Hostname: host}
	}

	path := filepath.Join(dir, instanceFile)
	data, err := os.ReadFile(path)
	switch {
	case err == nil && strings.TrimSpace(string(data)) != "":
		id := strings.ToLower(strings.TrimSpace(string(data)))
		if !ValidInstanceID(id) {
			log.Printf("[instance] %s holds %q, not a valid instance id — provisioning without identity (fix or delete the file)", path, id)
			return Instance{Hostname: host}
		}
		return Instance{ID: id, Hostname: host}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		log.Printf("[instance] cannot read %s: %v — provisioning without identity", path, err)
		return Instance{Hostname: host}
	}
	// Missing — or empty, which carries no identity worth protecting (an
	// interrupted first write): create one.

	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[instance] cannot create %s: %v — provisioning without identity", dir, err)
		return Instance{Hostname: host}
	}
	id, err := persistInstanceID(path, newInstanceID(host))
	if err != nil {
		log.Printf("[instance] cannot write %s: %v — provisioning without identity", path, err)
		return Instance{Hostname: host}
	}
	if !ValidInstanceID(id) {
		log.Printf("[instance] %s holds %q, not a valid instance id — provisioning without identity (fix or delete the file)", path, id)
		return Instance{Hostname: host}
	}
	log.Printf("[instance] using instance id %q (%s)", id, path)
	return Instance{ID: id, Hostname: host}
}

// persistInstanceID writes id to path unless another process got there
// first, in which case that process's id is returned: two standalone
// instances starting at the same moment must end up with the one identity
// the file holds, never with one of them presenting an id that exists in no
// file. An empty file (interrupted earlier write) is taken over.
func persistInstanceID(path, id string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		if existing := strings.ToLower(strings.TrimSpace(string(data))); existing != "" {
			return existing, nil
		}
		// Exists but empty: nobody owns it, take it over.
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return id, nil
}

// newInstanceID builds "<host-slug>-<6 hex>" — readable in an admin list, and
// distinct across reinstalls of the same machine.
func newInstanceID(host string) string {
	slug := slugify(host, instanceHostMaxLen)
	if slug == "" {
		slug = "mux"
	}
	b := make([]byte, 3)
	rand.Read(b) // never fails since Go 1.24 (it crashes the program instead)
	return slug + "-" + hex.EncodeToString(b)
}

// shortHostname returns the machine's host name without domain, reduced to
// printable ASCII so it is safe in an HTTP header.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	var b strings.Builder
	for _, r := range h {
		if r >= 0x20 && r <= 0x7e {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// slugify lowercases s and collapses every run of characters outside
// [a-z0-9] into a single dash, trimmed to at most max bytes.
func slugify(s string, max int) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}
