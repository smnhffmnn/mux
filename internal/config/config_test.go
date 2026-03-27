package config

import (
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
