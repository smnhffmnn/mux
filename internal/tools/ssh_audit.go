package tools

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Audit trail for the ssh connection type.
//
// Every command and file copy a connection runs — and every one it refuses —
// is written to the mux log and kept in a bounded in-memory ring that
// GET /audit/ssh on the loopback listener serves as JSON. The log line is the
// durable record (mux.log rotates at a few megabytes); the endpoint is the
// quick answer to "what did the agent just run on that host" without grepping.
// The ring holds entries since the process started.

const sshAuditSize = 500

// SSHAuditEntry is one logged operation.
type SSHAuditEntry struct {
	Time       time.Time `json:"time"`
	Connection string    `json:"connection"`
	Target     string    `json:"target"` // user@host:port
	Tool       string    `json:"tool"`   // exec, upload, download
	// Command is the command line as sent (exec) or "local → remote" /
	// "remote → local" (file copies).
	Command string `json:"command"`
	// Refused holds the policy reason when the operation never ran.
	Refused    string `json:"refused,omitempty"`
	DurationMS int64  `json:"durationMs"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	// Bytes counts stdout (exec) or the file (copies); StderrBytes is exec only.
	Bytes       int64  `json:"bytes"`
	StderrBytes int64  `json:"stderrBytes,omitempty"`
	Error       string `json:"error,omitempty"`
}

type sshAuditRing struct {
	mu      sync.Mutex // guards entries, next and full
	entries []SSHAuditEntry
	next    int
	full    bool
}

// reset empties the ring; tests use it to assert exact counts.
func (r *sshAuditRing) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make([]SSHAuditEntry, len(r.entries))
	r.next, r.full = 0, false
}

var sshAudit = &sshAuditRing{entries: make([]SSHAuditEntry, sshAuditSize)}

// record logs the entry and keeps it in the ring.
func (r *sshAuditRing) record(e SSHAuditEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	log.Print(sshAuditLine(e))

	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % len(r.entries)
	if r.next == 0 {
		r.full = true
	}
}

// recent returns the newest entries first, at most limit, optionally for one
// connection.
func (r *sshAuditRing) recent(connection string, limit int) []SSHAuditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.entries)
	}
	out := make([]SSHAuditEntry, 0, min(limit, n))
	for i := 1; i <= n && len(out) < limit; i++ {
		e := r.entries[(r.next-i+len(r.entries))%len(r.entries)]
		if connection != "" && e.Connection != connection {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sshAuditLine renders the log line. Commands are free-form and may contain
// control characters; they are flattened so a command cannot forge further
// log lines.
func sshAuditLine(e SSHAuditEntry) string {
	var b strings.Builder
	b.WriteString("[ssh] ")
	b.WriteString(e.Connection)
	b.WriteString(" ")
	b.WriteString(e.Tool)
	b.WriteString(" ")
	b.WriteString(strconv.Quote(flattenControl(e.Command)))
	if e.Refused != "" {
		b.WriteString(" refused=")
		b.WriteString(strconv.Quote(e.Refused))
		return b.String()
	}
	if e.ExitCode != nil {
		b.WriteString(" exit=")
		b.WriteString(strconv.Itoa(*e.ExitCode))
	}
	b.WriteString(" dur=")
	b.WriteString((time.Duration(e.DurationMS) * time.Millisecond).String())
	b.WriteString(" bytes=")
	b.WriteString(strconv.FormatInt(e.Bytes, 10))
	if e.StderrBytes > 0 {
		b.WriteString(" stderr=")
		b.WriteString(strconv.FormatInt(e.StderrBytes, 10))
	}
	if e.Error != "" {
		b.WriteString(" error=")
		b.WriteString(strconv.Quote(flattenControl(e.Error)))
	}
	return b.String()
}

func flattenControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// SSHAuditHandler serves GET /audit/ssh: the recent entries, newest first.
// Query parameters: connection (filter), limit (default 100, max ring size).
func SSHAuditHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = min(n, sshAuditSize)
			}
		}
		entries := sshAudit.recent(r.URL.Query().Get("connection"), limit)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"capacity": sshAuditSize,
			"count":    len(entries),
			"entries":  entries,
		})
	}
}

// MountSSHAudit registers the audit route on a loopback mux.
func MountSSHAudit(mux *http.ServeMux) {
	mux.HandleFunc("GET /audit/ssh", SSHAuditHandler())
}
