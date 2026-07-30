// Package logging routes the standard logger to a persistent log file so mux
// diagnostics survive GUI launches and stdio sessions. Before this existed,
// stdio instances discarded every log line and desktop instances wrote to a
// stderr nobody could see — leaving remote debugging (does the tunnel come up?
// which connections registered?) with no place to look.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	fileName = "mux.log"
	// maxSize is checked at startup only: a file that has outgrown it is
	// rotated aside to mux.log.1 (replacing the previous generation), so disk
	// use is bounded at roughly two generations without a rotation dependency
	// or size checks on the hot path.
	maxSize = 5 * 1024 * 1024
)

// Setup opens <dir>/logs/mux.log (rotating a grown file aside first) and
// wires the standard logger to it. In stdio mode the file is the only
// destination — stdout belongs to the MCP protocol and stderr stays as quiet
// as it always was. Otherwise log lines go to both stderr and the file.
// Every line carries the pid, because several instances (desktop app, stdio
// bridges) may share one file. Returns the log file path.
func Setup(dir string, stdio bool) (string, error) {
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(logDir, fileName)
	rotate(path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("open log file: %w", err)
	}

	if stdio {
		log.SetOutput(f)
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix(fmt.Sprintf("[%d] ", os.Getpid()))
	activePath = path
	return path, nil
}

// activePath is set once by Setup; main() runs before any reader (UI bindings),
// so no synchronization is needed.
var activePath string

// Path returns the active log file path, or "" when file logging is
// unavailable (Setup failed or was never called).
func Path() string { return activePath }

// rotate copies a log file that has outgrown maxSize to <path>.1 (replacing
// the previous generation) and truncates the original in place. Copy+truncate
// rather than rename, because another instance — typically the long-running
// desktop app — may hold the file open in O_APPEND mode: a rename would drag
// its file descriptor along to mux.log.1, and the next rotation would unlink
// that inode and silently discard its logs. After a truncate, O_APPEND
// writers simply continue at the new end of file. Lines written between copy
// and truncate are lost; for a diagnostics log that window is acceptable.
// Best-effort throughout: on any error the file just keeps growing until a
// later startup succeeds.
func rotate(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxSize {
		return
	}
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(path+".1", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return
	}
	if err := dst.Close(); err != nil {
		return
	}
	_ = os.Truncate(path, 0)
}
