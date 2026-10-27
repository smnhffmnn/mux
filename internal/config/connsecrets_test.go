package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDeleteConnectionSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if got := Dir(); !strings.HasPrefix(got, home) {
		t.Skipf("Dir() = %q is outside the temporary home on this platform", got)
	}
	prev := skipKeyring.Load()
	skipKeyring.Store(true) // file store only — never the developer's keychain
	t.Cleanup(func() { skipKeyring.Store(prev) })

	for _, key := range []string{"mail-password", "mail-token", "mail-oauth-token", "mail-oauth-refresh-token", "other-password"} {
		if err := fileSet(key, "s3cret"); err != nil {
			t.Fatal(err)
		}
	}
	// "mail-oauth" is another connection: its token key equals mail's OAuth token key.
	remaining := []Connection{{Name: "other"}, {Name: "mail-oauth"}}

	got := DeleteConnectionSecrets("mail", remaining)
	want := []string{"mail-password", "mail-token", "mail-oauth-refresh-token"}
	if !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	for _, key := range want {
		if v, err := getSecret(key); err == nil && v != "" {
			t.Errorf("%s survived the delete", key)
		}
	}
	for _, key := range []string{"other-password", "mail-oauth-token"} {
		if v, err := getSecret(key); err != nil || v != "s3cret" {
			t.Errorf("%s of a remaining connection was touched: %q, %v", key, v, err)
		}
	}
}
