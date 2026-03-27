package tunnel

import (
	"strings"
	"testing"
)

func TestDefaultKnownHostsPath(t *testing.T) {
	path := defaultKnownHostsPath()
	if path == "" {
		t.Skip("could not determine home directory")
	}
	if !strings.Contains(path, ".ssh/known_hosts") {
		t.Errorf("expected path containing .ssh/known_hosts, got %q", path)
	}
}
