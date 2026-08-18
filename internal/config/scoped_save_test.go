package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempConfigHome points the default config path at a throwaway directory.
func tempConfigHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	return DefaultConfigPath()
}

func newTestConfig() *Config {
	return &Config{Server: ServerConfig{Port: DefaultPort}}
}

// The core MUX-3 scenario: two full instances hold independent in-memory
// views of the same file. With whole-file saves, the second (stale) writer
// silently discarded the first writer's entry — scoped saves must keep both.
func TestScopedSavePreservesForeignChanges(t *testing.T) {
	path := tempConfigHome(t)

	cfgA := newTestConfig()
	connA := Connection{Name: "a", Type: TypeHTTP, URL: "https://a.example", Headers: map[string]string{"X-Version": "1"}}
	cfgA.Connections = append(cfgA.Connections, connA)
	if err := cfgA.SaveConnectionEntry(connA); err != nil {
		t.Fatalf("writer A: %v", err)
	}

	// Writer B's view predates A's save and never contains "a".
	cfgB := newTestConfig()
	connB := Connection{Name: "b", Type: TypeHTTP, URL: "https://b.example"}
	cfgB.Connections = append(cfgB.Connections, connB)
	if err := cfgB.SaveConnectionEntry(connB); err != nil {
		t.Fatalf("writer B: %v", err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Connection{}
	for _, c := range disk.Connections {
		byName[c.Name] = c
	}
	a, ok := byName["a"]
	if !ok {
		t.Fatal("connection \"a\" was clobbered by writer B's stale view (last-writer-wins)")
	}
	if a.Headers["X-Version"] != "1" {
		t.Errorf("connection \"a\" lost its headers: %#v", a.Headers)
	}
	if _, ok := byName["b"]; !ok {
		t.Fatal("connection \"b\" missing after its own save")
	}
}

func TestScopedSaveUpsertsInPlace(t *testing.T) {
	path := tempConfigHome(t)

	cfg := newTestConfig()
	conn := Connection{Name: "db", Type: TypeMariaDB, Host: "old-host", User: "u"}
	if err := cfg.SaveConnectionEntry(conn); err != nil {
		t.Fatal(err)
	}
	conn.Host = "new-host"
	if err := cfg.SaveConnectionEntry(conn); err != nil {
		t.Fatal(err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Connections) != 1 {
		t.Fatalf("upsert duplicated the entry: %d connections on disk", len(disk.Connections))
	}
	if disk.Connections[0].Host != "new-host" {
		t.Errorf("Host = %q, want %q", disk.Connections[0].Host, "new-host")
	}
}

func TestScopedDeleteRemovesOnlyNamedEntry(t *testing.T) {
	path := tempConfigHome(t)

	cfg := newTestConfig()
	for _, name := range []string{"keep", "drop"} {
		if err := cfg.SaveConnectionEntry(Connection{Name: name, Type: TypeHTTP, URL: "https://x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.DeleteConnectionEntry("drop"); err != nil {
		t.Fatal(err)
	}
	// Deleting an entry that is already gone is not an error.
	if err := cfg.DeleteConnectionEntry("drop"); err != nil {
		t.Fatalf("second delete errored: %v", err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Connections) != 1 || disk.Connections[0].Name != "keep" {
		t.Fatalf("unexpected connections on disk: %#v", disk.Connections)
	}
}

func TestScopedSavePreservesServerSection(t *testing.T) {
	path := tempConfigHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := "[server]\nport = 7788\nstdio_proxy = \"never\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := newTestConfig() // in-memory view has the default port, NOT 7788
	if err := cfg.SaveConnectionEntry(Connection{Name: "x", Type: TypeHTTP, URL: "https://x"}); err != nil {
		t.Fatal(err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Server.Port != 7788 {
		t.Errorf("Server.Port = %d, want 7788 (disk value must win over the writer's in-memory view)", disk.Server.Port)
	}
	if disk.Server.StdioProxy != "never" {
		t.Errorf("Server.StdioProxy = %q, want %q", disk.Server.StdioProxy, "never")
	}
}

func TestScopedSaveSeedsDefaultPortOnFreshFile(t *testing.T) {
	path := tempConfigHome(t)

	cfg := newTestConfig()
	if err := cfg.SaveTunnelEntry(TunnelConfig{Name: "t1", Type: TunnelTypeSSH, Host: "h", Port: 22, User: "u"}); err != nil {
		t.Fatal(err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Server.Port has no omitzero — an unseeded fresh view would persist
	// port 0 and the next Load would bind port 0.
	if disk.Server.Port != DefaultPort {
		t.Errorf("Server.Port = %d, want default %d", disk.Server.Port, DefaultPort)
	}
	if len(disk.Tunnels) != 1 || disk.Tunnels[0].Name != "t1" {
		t.Fatalf("unexpected tunnels on disk: %#v", disk.Tunnels)
	}
}

func TestScopedSaveProvisioningUpsertByName(t *testing.T) {
	path := tempConfigHome(t)

	cfg := newTestConfig()
	if err := cfg.SaveProvisioningEntry(ProvisioningConfig{Endpoint: "https://default.example"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveProvisioningEntry(ProvisioningConfig{Name: "second", Endpoint: "https://second.example"}); err != nil {
		t.Fatal(err)
	}
	// Update the default (unnamed) endpoint — must not touch "second".
	if err := cfg.SaveProvisioningEntry(ProvisioningConfig{Endpoint: "https://default-new.example"}); err != nil {
		t.Fatal(err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Provisioning) != 2 {
		t.Fatalf("provisioning entries on disk = %d, want 2", len(disk.Provisioning))
	}
	byName := map[string]string{}
	for _, p := range disk.Provisioning {
		byName[p.Name] = p.Endpoint
	}
	if byName[""] != "https://default-new.example" {
		t.Errorf("default endpoint = %q, want the updated value", byName[""])
	}
	if byName["second"] != "https://second.example" {
		t.Errorf("named endpoint = %q, want unchanged", byName["second"])
	}
}

func TestScopedSaveMigratesLegacySections(t *testing.T) {
	path := tempConfigHome(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := "[mariadb]\nhost = \"legacy-host\"\nuser = \"legacy\"\ndatabase = \"legacy_db\"\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := newTestConfig()
	if err := cfg.SaveConnectionEntry(Connection{Name: "new", Type: TypeHTTP, URL: "https://new"}); err != nil {
		t.Fatal(err)
	}

	disk, err := readDiskConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Connection{}
	for _, c := range disk.Connections {
		byName[c.Name] = c
	}
	m, ok := byName["mariadb"]
	if !ok {
		t.Fatal("legacy [mariadb] section was dropped by the scoped save instead of being migrated")
	}
	if m.Host != "legacy-host" || m.Database != "legacy_db" {
		t.Errorf("migrated legacy connection lost fields: %#v", m)
	}
	if _, ok := byName["new"]; !ok {
		t.Fatal("newly saved connection missing")
	}
}

func TestScopedSaveNeverPersistsSecretsOrTmp(t *testing.T) {
	path := tempConfigHome(t)

	cfg := newTestConfig()
	conn := Connection{Name: "s", Type: TypeMariaDB, Host: "h", User: "u", Password: "super-secret-pw", Token: "super-secret-token"}
	if err := cfg.SaveConnectionEntry(conn); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal("secret field leaked into config.toml")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}
