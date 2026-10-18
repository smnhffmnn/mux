package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadInstance_CreatesAndPersistsAnID(t *testing.T) {
	t.Setenv(InstanceIDEnv, "")
	dir := filepath.Join(t.TempDir(), "mux")

	first := loadInstance(dir)
	if first.ID == "" {
		t.Fatal("expected an instance id to be generated")
	}
	if !ValidInstanceID(first.ID) {
		t.Errorf("generated id %q does not satisfy the slug rule", first.ID)
	}
	if !strings.HasSuffix(first.ID, "-"+first.ID[len(first.ID)-6:]) || len(first.ID) < 8 {
		t.Errorf("generated id %q should end in a 6-char random suffix", first.ID)
	}

	data, err := os.ReadFile(filepath.Join(dir, instanceFile))
	if err != nil {
		t.Fatalf("instance file not written: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != first.ID {
		t.Errorf("file holds %q, want %q", got, first.ID)
	}
	if runtime.GOOS != "windows" { // Windows reports 0666 for any writable file
		info, err := os.Stat(filepath.Join(dir, instanceFile))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("instance file is %v, want owner-only", perm)
		}
	}

	// A second resolution reuses the persisted id — that is the whole point:
	// a new id per start would register a new instance per start.
	second := loadInstance(dir)
	if second.ID != first.ID {
		t.Errorf("second load = %q, want the persisted %q", second.ID, first.ID)
	}
}

func TestLoadInstance_EnvWinsAndIsNotPersisted(t *testing.T) {
	t.Setenv(InstanceIDEnv, " PC-Container-01 ")
	dir := filepath.Join(t.TempDir(), "mux")

	got := loadInstance(dir)
	if got.ID != "pc-container-01" {
		t.Errorf("ID = %q, want the trimmed, lowercased env value", got.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, instanceFile)); !os.IsNotExist(err) {
		t.Error("the env id must not be written to the config dir")
	}
}

func TestLoadInstance_InvalidValuesYieldNoIdentity(t *testing.T) {
	t.Setenv(InstanceIDEnv, "not a slug!")
	if got := loadInstance(filepath.Join(t.TempDir(), "mux")); got.ID != "" {
		t.Errorf("invalid env id should give no identity, got %q", got.ID)
	}

	t.Setenv(InstanceIDEnv, "")
	dir := filepath.Join(t.TempDir(), "mux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, instanceFile), []byte("Has Spaces\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadInstance(dir)
	if got.ID != "" {
		t.Errorf("corrupt file should give no identity, got %q", got.ID)
	}
	// The corrupt file is left alone — silently replacing it would silently
	// become a different device.
	data, _ := os.ReadFile(filepath.Join(dir, instanceFile))
	if string(data) != "Has Spaces\n" {
		t.Errorf("corrupt file was rewritten to %q", data)
	}
}

func TestLoadInstance_ReadErrorYieldsNoIdentity(t *testing.T) {
	t.Setenv(InstanceIDEnv, "")
	// A regular file where the directory should be: ReadFile fails with
	// ENOTDIR, which is not "does not exist", so nothing is generated.
	blocker := filepath.Join(t.TempDir(), "mux")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadInstance(blocker); got.ID != "" {
		t.Errorf("a read error must not produce an identity, got %q", got.ID)
	}
}

func TestLoadInstance_UnwritableDirYieldsNoIdentity(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	t.Setenv(InstanceIDEnv, "")
	dir := filepath.Join(t.TempDir(), "mux")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if got := loadInstance(dir); got.ID != "" {
		t.Errorf("an id that cannot be persisted must not be reported, got %q", got.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, instanceFile)); !os.IsNotExist(err) {
		t.Error("no file may appear in an unwritable dir")
	}
}

func TestLoadInstance_MissingParentYieldsNoIdentity(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	t.Setenv(InstanceIDEnv, "")
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	// MkdirAll of the config dir itself fails.
	if got := loadInstance(filepath.Join(parent, "mux")); got.ID != "" {
		t.Errorf("an id whose dir cannot be created must not be reported, got %q", got.ID)
	}
}

func TestLoadInstance_EmptyFileIsTakenOver(t *testing.T) {
	t.Setenv(InstanceIDEnv, "")
	dir := filepath.Join(t.TempDir(), "mux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An interrupted first write leaves an empty file; it carries no identity
	// and must not strand the installation without one.
	if err := os.WriteFile(filepath.Join(dir, instanceFile), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadInstance(dir)
	if got.ID == "" {
		t.Fatal("empty file should be replaced by a generated id")
	}
	data, _ := os.ReadFile(filepath.Join(dir, instanceFile))
	if strings.TrimSpace(string(data)) != got.ID {
		t.Errorf("file holds %q, want %q", data, got.ID)
	}
}

func TestPersistInstanceID_FirstWriterWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), instanceFile)

	first, err := persistInstanceID(path, "host-aaaaaa")
	if err != nil || first != "host-aaaaaa" {
		t.Fatalf("first persist = %q, %v", first, err)
	}
	// A second process that lost the race must adopt the persisted id,
	// never present its own.
	second, err := persistInstanceID(path, "host-bbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if second != "host-aaaaaa" {
		t.Errorf("second persist = %q, want the first writer's id", second)
	}
	data, _ := os.ReadFile(path)
	if strings.TrimSpace(string(data)) != "host-aaaaaa" {
		t.Errorf("file holds %q, want the first writer's id", data)
	}
}

func TestValidInstanceID(t *testing.T) {
	for _, ok := range []string{"a", "macbook-pro-1a2b3c", "pc_container", "x1-y2_z3", strings.Repeat("a", instanceIDMaxLen)} {
		if !ValidInstanceID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-lead", "trail-", "double--dash", "Upper", "dot.ted", "ümlaut", strings.Repeat("a", instanceIDMaxLen+1)} {
		if ValidInstanceID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Simons-MacBook-Pro": "simons-macbook-pro",
		"  PC  01 ":          "pc-01",
		"Büro-PC":            "b-ro-pc",
		"---":                "",
		"":                   "",
	}
	for in, want := range cases {
		if got := slugify(in, 40); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := slugify(strings.Repeat("ab-", 30), 10); len(got) > 10 || strings.HasSuffix(got, "-") {
		t.Errorf("slugify should cap the length and never end in a dash, got %q", got)
	}
}

func TestNewInstanceID_FallsBackToMuxForEmptyHost(t *testing.T) {
	id := newInstanceID("")
	if !strings.HasPrefix(id, "mux-") || !ValidInstanceID(id) {
		t.Errorf("newInstanceID(\"\") = %q", id)
	}
}
