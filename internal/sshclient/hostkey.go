package sshclient

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyPolicy decides which server key is accepted. Exactly one of the
// three applies, in this order: a pinned key, the known_hosts files, or —
// for tunnels that opted in — nothing.
type HostKeyPolicy struct {
	// Pinned is one public key in authorized_keys form ("ssh-ed25519 AAAA…
	// comment") or as a known_hosts line. When set, only this key is accepted
	// and the files are not consulted. A provisioning server that knows its
	// hosts delivers the key here.
	Pinned string
	// KnownHosts lists the files consulted when Pinned is empty. Files that
	// do not exist are skipped; when none exists, every host is unknown and
	// the error says which line to add where.
	KnownHosts []string
	// Insecure accepts any key. Kept for the tunnel's insecure_host_key flag;
	// the connection type has no way to set it.
	Insecure bool
	// Logf receives a line per known_hosts entry that had to be skipped;
	// nil = log.Printf.
	Logf func(format string, args ...any)
}

// DefaultKnownHostsFiles returns the files mux consults: its own
// known_hosts in the config directory first (the one provisioning and
// containers can manage), then the user's ~/.ssh/known_hosts.
func DefaultKnownHostsFiles(configDir string) []string {
	var files []string
	if configDir != "" {
		files = append(files, filepath.Join(configDir, "known_hosts"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	return files
}

// ParsePublicKey reads a host key as written in authorized_keys
// ("ssh-ed25519 AAAA… comment") or in known_hosts ("host ssh-ed25519 AAAA…").
// Used to validate a host_key value where it is entered.
func ParsePublicKey(line string) (ssh.PublicKey, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("empty host key")
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("host_key must be a single line")
	}
	if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
		return key, nil
	}
	if _, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line)); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("host_key is neither an authorized_keys line (\"ssh-ed25519 AAAA…\") nor a known_hosts line")
}

// HostKeyVerifier is a policy resolved for use: the callback that checks the
// server's key, a description for logs ("pinned ssh-ed25519 SHA256:…",
// "known_hosts: /a, /b"), and the host key algorithms to offer.
type HostKeyVerifier struct {
	Callback    ssh.HostKeyCallback
	Description string
	// Algorithms returns the host key algorithms to offer when connecting to
	// addr (host:port): the algorithms of the keys already known for the host
	// first, then the library defaults. nil when nothing is known.
	//
	// This is what OpenSSH does, and without it a known host fails as a
	// mismatch: a server usually holds an RSA, an ECDSA and an Ed25519 key and
	// presents whichever the client lists first. x/crypto lists Ed25519 last,
	// while OpenSSH clients prefer it — so a typical ~/.ssh/known_hosts has the
	// Ed25519 key, and a client that does not ask for it is shown ECDSA.
	Algorithms func(addr string) []string
}

// Verifier resolves the policy. It validates a pinned key and parses the
// known_hosts files once.
func (p HostKeyPolicy) Verifier() (*HostKeyVerifier, error) {
	if strings.TrimSpace(p.Pinned) != "" {
		pinned, err := ParsePublicKey(p.Pinned)
		if err != nil {
			return nil, err
		}
		want := pinned.Marshal()
		fp := ssh.FingerprintSHA256(pinned)
		cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if bytes.Equal(key.Marshal(), want) {
				return nil
			}
			return fmt.Errorf("HOST KEY MISMATCH for %s: the server presented %s %s, the connection pins %s %s — refusing (a reinstalled host needs a new host_key; anything else is a man-in-the-middle)",
				hostname, key.Type(), ssh.FingerprintSHA256(key), pinned.Type(), fp)
		}
		return &HostKeyVerifier{
			Callback:    cb,
			Description: fmt.Sprintf("pinned %s %s", pinned.Type(), fp),
			Algorithms:  func(string) []string { return preferAlgorithms([]string{pinned.Type()}) },
		}, nil
	}

	if p.Insecure {
		return &HostKeyVerifier{Callback: ssh.InsecureIgnoreHostKey(), Description: "host key verification disabled"}, nil
	}

	var existing []string
	for _, f := range p.KnownHosts {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			existing = append(existing, f)
		}
	}
	desc := "known_hosts: " + strings.Join(p.KnownHosts, ", ")
	if len(existing) == 0 {
		// Nothing to check against: every host is unknown. Not an error at
		// construction — the message at connect time carries the line to add.
		files := p.KnownHosts
		cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return unknownHostError(hostname, key, files)
		}
		return &HostKeyVerifier{Callback: cb, Description: desc + " (none present)"}, nil
	}

	logf := p.Logf
	if logf == nil {
		logf = log.Printf
	}
	inner, origin, err := loadKnownHosts(existing, logf)
	if err != nil {
		return nil, err
	}
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := inner(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err
		}
		if len(ke.Want) == 0 {
			return unknownHostError(hostname, key, p.KnownHosts)
		}
		var known []string
		for _, w := range ke.Want {
			file := w.Filename
			if orig, ok := origin[file]; ok {
				file = orig
			}
			known = append(known, fmt.Sprintf("%s %s (%s:%d)", w.Key.Type(), ssh.FingerprintSHA256(w.Key), file, w.Line))
		}
		return fmt.Errorf("HOST KEY MISMATCH for %s: the server presented %s %s, known_hosts has %s — refusing (a reinstalled host needs its old line removed; anything else is a man-in-the-middle)",
			hostname, key.Type(), ssh.FingerprintSHA256(key), strings.Join(known, "; "))
	}
	return &HostKeyVerifier{
		Callback:    cb,
		Description: desc,
		Algorithms:  func(addr string) []string { return knownAlgorithms(inner, addr) },
	}, nil
}

// knownAlgorithms asks the knownhosts database which keys it holds for addr.
// The database has no lookup API, but its callback reports the keys it
// expected in KeyError.Want when a key does not match — so probing it with a
// throwaway key yields exactly the list. No match: nothing known, nil.
func knownAlgorithms(cb ssh.HostKeyCallback, addr string) []string {
	probe := probeKey()
	if probe == nil {
		return nil // library default order; verification is unaffected
	}
	err := cb(addr, probeAddr(addr), probe)
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil
	}
	var types []string
	for _, w := range ke.Want {
		types = append(types, w.Key.Type())
	}
	return preferAlgorithms(types)
}

// preferAlgorithms orders the host key algorithms to offer: the ones for the
// known key types first (an ssh-rsa key is usable through the rsa-sha2
// signature algorithms as well), then the library's defaults, so an unknown
// or re-keyed host still negotiates and is then reported as a mismatch.
func preferAlgorithms(knownTypes []string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, t := range knownTypes {
		switch t {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSA)
		default:
			add(t)
		}
	}
	for _, a := range ssh.SupportedAlgorithms().HostKeys {
		add(a)
	}
	return out
}

// probeAddr satisfies the net.Addr the knownhosts callback splits into host
// and port.
type probeAddr string

func (a probeAddr) Network() string { return "tcp" }
func (a probeAddr) String() string  { return string(a) }

var (
	probeKeyOnce sync.Once
	probeKeyVal  ssh.PublicKey
)

// probeKey is a throwaway key no known_hosts file contains, used to make the
// knownhosts callback reveal what it does contain for a host. nil when the
// key could not be generated — then callers keep the library's default order.
func probeKey() ssh.PublicKey {
	probeKeyOnce.Do(func() {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return
		}
		if key, err := ssh.NewPublicKey(pub); err == nil {
			probeKeyVal = key
		}
	})
	return probeKeyVal
}

// loadKnownHosts builds the knownhosts callback from files, skipping lines the
// parser rejects. OpenSSH ignores a malformed line; x/crypto's knownhosts.New
// refuses the whole file — and a user's ~/.ssh/known_hosts does accumulate
// junk (an ssh error message appended by a script, say). One bad line must not
// take every SSH connection down with it.
//
// The filter uses knownhosts.New itself as the judge, not a stricter
// pre-parser: the content goes into a temporary file, and each time New
// reports "file:line: error" that line is blanked and New runs again. So a
// line is dropped exactly when the verifying parser would have refused it —
// a trailing multi-word comment, which the parser accepts, stays. Blanking
// rather than deleting keeps line numbers aligned with the original for
// mismatch reports; origin maps the temporary names back for those reports.
func loadKnownHosts(files []string, logf func(string, ...any)) (ssh.HostKeyCallback, map[string]string, error) {
	origin := make(map[string]string, len(files))
	var tmpFiles []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}()

	// Every file goes through a temporary copy, so a file that turns out to
	// need no blanking costs one copy and nothing else.
	var copies []*knownHostsCopy
	byTmp := make(map[string]*knownHostsCopy)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, fmt.Errorf("read known_hosts %s: %w", file, err)
		}
		tmp, err := os.CreateTemp("", "mux-known_hosts-*")
		if err != nil {
			return nil, nil, fmt.Errorf("prepare known_hosts %s: %w", file, err)
		}
		tmp.Close()
		tmpFiles = append(tmpFiles, tmp.Name())
		c := &knownHostsCopy{orig: file, tmp: tmp.Name(), lines: strings.Split(string(data), "\n")}
		copies = append(copies, c)
		byTmp[c.tmp] = c
		origin[c.tmp] = file
	}

	var load []string
	for _, c := range copies {
		load = append(load, c.tmp)
	}
	for attempt := 0; ; attempt++ {
		for _, c := range copies {
			if err := os.WriteFile(c.tmp, []byte(strings.Join(c.lines, "\n")), 0o600); err != nil {
				return nil, nil, fmt.Errorf("prepare known_hosts %s: %w", c.orig, err)
			}
		}
		cb, err := knownhosts.New(load...)
		if err == nil {
			return cb, origin, nil
		}
		tmp, line, ok := parseKnownHostsError(err.Error(), byTmp)
		if !ok || attempt > 10000 {
			msg := err.Error()
			for t, o := range origin {
				msg = strings.ReplaceAll(msg, t, o)
			}
			return nil, nil, fmt.Errorf("parse known_hosts: %s", msg)
		}
		c := byTmp[tmp]
		if line < 1 || line > len(c.lines) || c.lines[line-1] == "" {
			return nil, nil, fmt.Errorf("parse known_hosts %s: %v", c.orig, err)
		}
		if logf != nil {
			logf("[ssh] known_hosts %s:%d: skipping line the parser rejects (%v)", c.orig, line, trimKnownHostsError(err.Error(), tmp, line))
		}
		c.lines[line-1] = ""
	}
}

// parseKnownHostsError extracts file and line from knownhosts.New's error,
// which reads "knownhosts: <file>:<line>: <reason>". The file is one of the
// temporary copies.
func parseKnownHostsError(msg string, byTmp map[string]*knownHostsCopy) (string, int, bool) {
	for tmp := range byTmp {
		idx := strings.Index(msg, tmp+":")
		if idx < 0 {
			continue
		}
		rest := msg[idx+len(tmp)+1:]
		end := strings.IndexByte(rest, ':')
		if end < 0 {
			continue
		}
		line, err := strconv.Atoi(rest[:end])
		if err != nil {
			continue
		}
		return tmp, line, true
	}
	return "", 0, false
}

// trimKnownHostsError reduces the parser's error to its reason.
func trimKnownHostsError(msg, tmp string, line int) string {
	prefix := fmt.Sprintf("%s:%d: ", tmp, line)
	if idx := strings.Index(msg, prefix); idx >= 0 {
		return msg[idx+len(prefix):]
	}
	return msg
}

// knownHostsCopy is one known_hosts file as loadKnownHosts works on it.
type knownHostsCopy struct {
	orig  string
	tmp   string
	lines []string
}

// unknownHostError is the error for a host no file knows. It carries the
// known_hosts line verbatim, so trusting the host is a paste, not a
// transcription of a fingerprint.
func unknownHostError(hostname string, key ssh.PublicKey, files []string) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	return fmt.Errorf("host key of %s is not known (%s %s). Checked: %s. To trust this host, add the line below to the first of those files (an ssh connection can pin it with host_key instead); the next attempt re-reads them:\n%s",
		hostname, key.Type(), ssh.FingerprintSHA256(key), strings.Join(nonEmpty(files), ", "), line)
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{"(no known_hosts files configured)"}
	}
	return out
}
