package config

import (
	"runtime"
	"testing"
)

func TestParseCommandPatterns(t *testing.T) {
	pats, err := ParseCommandPatterns([]string{" cat ", "", "systemctl   status", "journalctl --vacuum*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pats) != 3 {
		t.Fatalf("got %d patterns, want 3", len(pats))
	}
	if got := pats[1].Tokens; len(got) != 2 || got[0] != "systemctl" || got[1] != "status" {
		t.Errorf("tokens = %v", got)
	}
	for _, bad := range []string{"*", "* --help", "**"} {
		if _, err := ParseCommandPatterns([]string{bad}); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestCommandPattern_Matches(t *testing.T) {
	cases := []struct {
		pattern string
		argv    []string
		want    bool
	}{
		{"cat", []string{"cat", "/etc/hosts"}, true},
		{"cat", []string{"/bin/cat", "/etc/hosts"}, false}, // the program is matched as given
		{"cat", []string{"/tmp/evil/cat"}, false},
		{"/bin/cat", []string{"/bin/cat", "x"}, true},
		{"cat", []string{"cat"}, true},
		{"cat", []string{"catalog"}, false},
		{"cat", []string{"sudo", "cat"}, false},
		{"cat", nil, false},
		{"systemctl status", []string{"systemctl", "status", "nginx"}, true},
		{"systemctl status", []string{"systemctl", "restart", "nginx"}, false},
		{"systemctl status", []string{"systemctl"}, false},
		{"journalctl --vacuum*", []string{"journalctl", "--vacuum-size=1G"}, true},
		{"journalctl --vacuum*", []string{"journalctl", "-u", "nginx"}, false},
		{"docker logs", []string{"/usr/bin/docker", "logs", "-f", "web"}, false},
		{"docker logs", []string{"docker", "logs", "-f", "web"}, true},
		{"ls /var/*", []string{"ls", "/var/log/nginx"}, true},
		{"ls /var/?", []string{"ls", "/var/x"}, true},
		{"ls /var/?", []string{"ls", "/var/xy"}, false},
	}
	for _, c := range cases {
		pats, err := ParseCommandPatterns([]string{c.pattern})
		if err != nil {
			t.Fatal(err)
		}
		if got := pats[0].Matches(c.argv); got != c.want {
			t.Errorf("%q vs %v = %v, want %v", c.pattern, c.argv, got, c.want)
		}
	}
}

func TestValidSecretKey_SSH(t *testing.T) {
	for _, ok := range []string{"deploy-private-key", "deploy-private-key-passphrase", "tunnel-bastion-private-key", "tunnel-bastion-private-key-passphrase", "tunnel-office-preshared-key"} {
		if !ValidSecretKey(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"-private-key", "-private-key-passphrase", "tunnel--preshared-key", "deploy-preshared-key"} {
		if ValidSecretKey(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestTunnelConfig_Enabled_Agent(t *testing.T) {
	tun := TunnelConfig{Name: "b", Type: TunnelTypeSSH, Host: "h", User: "u"}
	if tun.Enabled() {
		t.Error("ssh tunnel without key must not be enabled")
	}
	tun.Auth = "agent"
	if !tun.Enabled() {
		t.Error("ssh tunnel with auth=agent must be enabled without a stored key")
	}
}

func TestSetProvisioned_ClearsInsecureHostKey(t *testing.T) {
	cfg := &Config{Provisioning: []ProvisioningConfig{{Name: "erp", Endpoint: "https://erp.example/p", Token: "t"}}}
	cfg.SetProvisioned("erp", []TunnelConfig{{Name: "b", Type: TunnelTypeSSH, Host: "h", User: "u", InsecureHostKey: true, PrivateKey: "k"}}, nil)
	if tuns := cfg.ProvisionedTunnels(); len(tuns) != 1 || tuns[0].InsecureHostKey {
		t.Fatalf("insecure_host_key must be cleared on provisioned tunnels: %+v", tuns)
	}
}

func TestConnection_Enabled_SSH(t *testing.T) {
	c := Connection{Type: TypeSSH, Host: "h", User: "u"}
	if c.Enabled() {
		t.Error("ssh without key, key_file or agent must not be enabled")
	}
	c.Auth = "agent"
	if !c.Enabled() {
		t.Error("ssh with auth=agent must be enabled")
	}
	c.Auth = ""
	c.KeyFile = "~/.ssh/id_ed25519"
	if !c.Enabled() {
		t.Error("ssh with key_file must be enabled")
	}
	c.KeyFile = ""
	c.PrivateKey = "pem"
	if !c.Enabled() {
		t.Error("ssh with private key must be enabled")
	}
	ApplyConnectionDefaults(&c)
	if c.Port != 22 {
		t.Errorf("default port = %d, want 22", c.Port)
	}
}

// TestSetProvisioned_SSHSecretsFromStore: a provisioned ssh connection or
// tunnel that arrives without key material picks the key and its passphrase up
// from the local secret store — the shape where the server knows the host and
// the user keeps the key.
func TestSetProvisioned_SSHSecretsFromStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG_CONFIG_HOME is not honored on Windows")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prev := skipKeyring.Load()
	skipKeyring.Store(true) // file store only — never the developer's keychain
	t.Cleanup(func() { skipKeyring.Store(prev) })

	for k, v := range map[string]string{
		"web01-private-key":                     "PEM",
		"web01-private-key-passphrase":          "pp",
		"tunnel-bastion-private-key":            "TPEM",
		"tunnel-bastion-private-key-passphrase": "tpp",
	} {
		if err := fileSet(k, v); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &Config{Provisioning: []ProvisioningConfig{{Name: "erp", Endpoint: "https://erp.example/api/mux/provision", Token: "t"}}}
	cfg.SetProvisioned("erp",
		[]TunnelConfig{{Name: "bastion", Type: TunnelTypeSSH, Host: "b", User: "u"}},
		[]Connection{{Name: "web01", Type: TypeSSH, Host: "h", User: "u"}},
	)
	conns := cfg.ProvisionedConnections()
	if len(conns) != 1 || conns[0].PrivateKey != "PEM" || conns[0].Passphrase != "pp" {
		t.Fatalf("provisioned ssh connection did not pick up its secrets: %+v", conns)
	}
	if !conns[0].Enabled() || conns[0].Port != 22 {
		t.Errorf("provisioned ssh connection should be enabled with port 22, got enabled=%v port=%d", conns[0].Enabled(), conns[0].Port)
	}
	tuns := cfg.ProvisionedTunnels()
	if len(tuns) != 1 || tuns[0].PrivateKey != "TPEM" || tuns[0].Passphrase != "tpp" {
		t.Fatalf("provisioned ssh tunnel did not pick up its secrets: %+v", tuns)
	}
}
