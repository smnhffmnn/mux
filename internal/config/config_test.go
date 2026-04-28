package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExpandHome(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(string) bool
	}{
		{
			name:  "tilde slash path expands",
			input: "~/.ssh/id_rsa",
			check: func(s string) bool { return s != "~/.ssh/id_rsa" && len(s) > len("~/.ssh/id_rsa") },
		},
		{
			name:  "bare tilde expands",
			input: "~",
			check: func(s string) bool { return s != "~" && len(s) > 1 },
		},
		{
			name:  "absolute path unchanged",
			input: "/etc/ssh/key",
			check: func(s string) bool { return s == "/etc/ssh/key" },
		},
		{
			name:  "relative path unchanged",
			input: "keys/id_rsa",
			check: func(s string) bool { return s == "keys/id_rsa" },
		},
		{
			name:  "empty string unchanged",
			input: "",
			check: func(s string) bool { return s == "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandHome(tt.input)
			if !tt.check(got) {
				t.Errorf("ExpandHome(%q) = %q", tt.input, got)
			}
		})
	}
}

func TestTunnelConfigEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  TunnelConfig
		want bool
	}{
		{
			name: "WireGuard complete",
			cfg:  TunnelConfig{PrivateKey: "key", PeerPublicKey: "pub", PeerEndpoint: "1.2.3.4:51820"},
			want: true,
		},
		{
			name: "WireGuard missing key",
			cfg:  TunnelConfig{PeerPublicKey: "pub", PeerEndpoint: "1.2.3.4:51820"},
			want: false,
		},
		{
			name: "SSH with private key",
			cfg:  TunnelConfig{Type: "ssh", Host: "host", User: "user", PrivateKey: "key"},
			want: true,
		},
		{
			name: "SSH with key file",
			cfg:  TunnelConfig{Type: "ssh", Host: "host", User: "user", KeyFile: "~/.ssh/id_rsa"},
			want: true,
		},
		{
			name: "SSH missing host",
			cfg:  TunnelConfig{Type: "ssh", User: "user", PrivateKey: "key"},
			want: false,
		},
		{
			name: "SSH missing user",
			cfg:  TunnelConfig{Type: "ssh", Host: "host", PrivateKey: "key"},
			want: false,
		},
		{
			name: "SSH missing key and keyfile",
			cfg:  TunnelConfig{Type: "ssh", Host: "host", User: "user"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Enabled()
			if got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTunnelConfigIsSSH(t *testing.T) {
	if (&TunnelConfig{Type: "ssh"}).IsSSH() != true {
		t.Error("Type=ssh should be SSH")
	}
	if (&TunnelConfig{Type: ""}).IsSSH() != false {
		t.Error("Type=empty should not be SSH")
	}
	if (&TunnelConfig{Type: "wireguard"}).IsSSH() != false {
		t.Error("Type=wireguard should not be SSH")
	}
}

func TestConnectionEnabled_IMAP(t *testing.T) {
	tests := []struct {
		name string
		conn Connection
		want bool
	}{
		{
			name: "IMAP complete",
			conn: Connection{Type: "imap", Host: "imap.x.com", User: "user", Password: "pass"},
			want: true,
		},
		{
			name: "IMAP missing password",
			conn: Connection{Type: "imap", Host: "imap.x.com", User: "user"},
			want: false,
		},
		{
			name: "IMAP missing host",
			conn: Connection{Type: "imap", User: "user", Password: "pass"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.conn.Enabled()
			if got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidType_IMAP(t *testing.T) {
	if !ValidType("imap") {
		t.Error("imap should be a valid type")
	}
}

func TestApplyConnectionDefaults_IMAP(t *testing.T) {
	c := Connection{Type: "imap"}
	ApplyConnectionDefaults(&c)
	if c.Port != 993 {
		t.Errorf("IMAP default port should be 993, got %d", c.Port)
	}
}

func TestDir_PreferenceAndFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dir uses ~/.mux on Windows; XDG fallback test does not apply")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	preferred := filepath.Join(home, ".config", "mux")
	legacy := filepath.Join(home, ".mux")

	// Neither path exists → return preferred so first-write lands there.
	if got := Dir(); got != preferred {
		t.Errorf("empty home: Dir() = %q, want %q", got, preferred)
	}

	// Legacy exists, preferred does not → return legacy so existing
	// installations keep finding their data.
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != legacy {
		t.Errorf("legacy only: Dir() = %q, want %q", got, legacy)
	}

	// Preferred exists → always wins, regardless of legacy.
	if err := os.MkdirAll(preferred, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != preferred {
		t.Errorf("both exist: Dir() = %q, want %q", got, preferred)
	}
}

func TestDir_XDGConfigHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG_CONFIG_HOME is not honored on Windows")
	}

	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := filepath.Join(xdg, "mux")
	if got := Dir(); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestMigrateLegacyDir_MovesLegacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("migration is non-Windows only")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	legacy := filepath.Join(home, ".mux")
	preferred := filepath.Join(home, ".config", "mux")

	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, "config.toml")
	if err := os.WriteFile(marker, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	MigrateLegacyDir()

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy %s should be gone, stat err = %v", legacy, err)
	}
	moved := filepath.Join(preferred, "config.toml")
	data, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("expected file at %s, got err %v", moved, err)
	}
	if string(data) != "hello" {
		t.Errorf("file content = %q, want %q", data, "hello")
	}
}

func TestMigrateLegacyDir_NoLegacyIsNoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("migration is non-Windows only")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	MigrateLegacyDir()

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("home should remain empty, has %d entries", len(entries))
	}
}

func TestMigrateLegacyDir_BothExistKeepsBoth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("migration is non-Windows only")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	legacy := filepath.Join(home, ".mux")
	preferred := filepath.Join(home, ".config", "mux")
	for _, p := range []string{legacy, preferred} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacyMarker := filepath.Join(legacy, "old.toml")
	preferredMarker := filepath.Join(preferred, "new.toml")
	for _, p := range []string{legacyMarker, preferredMarker} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	MigrateLegacyDir()

	for _, p := range []string{legacyMarker, preferredMarker} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should still exist: %v", p, err)
		}
	}
}
