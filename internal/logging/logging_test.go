package logging

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetLogger restores the standard logger so a test cannot leak a file
// writer into the rest of the suite.
func resetLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
		log.SetPrefix("")
		activePath = ""
	})
}

func TestSetupCreatesFileAndWritesThroughLogger(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()

	path, err := Setup(dir, true) // stdio: file only
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if want := filepath.Join(dir, "logs", "mux.log"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if Path() != path {
		t.Fatalf("Path() = %q, want %q", Path(), path)
	}

	log.Printf("hello from test")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "hello from test") {
		t.Fatalf("log file does not contain the written line: %q", data)
	}
}

func TestSetupNonStdioKeepsStderr(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()

	if _, err := Setup(dir, false); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// stderr itself is not capturable here without dup2 tricks; assert the
	// file half of the multi-writer via the shared logger.
	log.Printf("dual sink line")
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "dual sink line") {
		t.Fatalf("log file does not contain the written line: %q", data)
	}
}

func TestRotateMovesOversizedFile(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "mux.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxSize+1), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Setup(dir, true); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if rotated.Size() != int64(maxSize+1) {
		t.Fatalf("rotated size = %d, want %d", rotated.Size(), maxSize+1)
	}
	fresh, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fresh log missing: %v", err)
	}
	if fresh.Size() != 0 {
		t.Fatalf("fresh log size = %d, want 0", fresh.Size())
	}
}
