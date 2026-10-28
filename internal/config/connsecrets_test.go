package config

import (
	"errors"
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

	for _, key := range []string{"mail-password", "mail-token", "mail-oauth-token", "mail-oauth-refresh-token",
		"mail-oauth-client-id", "mail-oauth-client-secret", "other-password"} {
		if err := fileSet(key, "s3cret"); err != nil {
			t.Fatal(err)
		}
	}
	// "mail-oauth" is another connection: its token key equals mail's OAuth token key.
	remaining := []Connection{{Name: "other"}, {Name: "mail-oauth"}}

	got, err := DeleteConnectionSecrets("mail", remaining)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mail-password", "mail-token", "mail-oauth-refresh-token", "mail-oauth-client-id", "mail-oauth-client-secret"}
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

func TestDeleteConnectionSecretsKeepsReservedNamespaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if got := Dir(); !strings.HasPrefix(got, home) {
		t.Skipf("Dir() = %q is outside the temporary home on this platform", got)
	}
	prev := skipKeyring.Load()
	skipKeyring.Store(true)
	t.Cleanup(func() { skipKeyring.Store(prev) })

	// Keys of a provisioning endpoint and a tunnel, spelled like the keys of
	// connections named "provisioning" and "tunnel-bastion".
	for _, key := range []string{"provisioning-token", "tunnel-bastion-private-key", "vault-telegram-bot-token"} {
		if err := fileSet(key, "keep"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"provisioning", "tunnel-bastion", "vault-telegram-bot"} {
		if got, err := DeleteConnectionSecrets(name, nil); err != nil || len(got) != 0 {
			t.Errorf("%s: deleted %v (%v), want nothing", name, got, err)
		}
	}
	for _, key := range []string{"provisioning-token", "tunnel-bastion-private-key", "vault-telegram-bot-token"} {
		if v, err := fileGet(key); err != nil || v != "keep" {
			t.Errorf("%s was touched: %q, %v", key, v, err)
		}
	}
}

// lockedVault is a vault that is set up and locked.
type lockedVault struct{}

func (lockedVault) IsUnlocked() bool                 { return false }
func (lockedVault) GetSecret(string) (string, error) { return "", errors.New("vault is sealed") }
func (lockedVault) SetSecret(string, string) error   { return errors.New("vault is sealed") }
func (lockedVault) DeleteSecret(string) error        { return errors.New("vault is sealed") }

func TestDeleteConnectionSecretsRefusesALockedVault(t *testing.T) {
	SetActiveVault(lockedVault{})
	t.Cleanup(func() { SetActiveVault(nil) })

	if err := ConnectionSecretsDeletable(); !errors.Is(err, ErrVaultLocked) {
		t.Errorf("ConnectionSecretsDeletable() = %v, want ErrVaultLocked", err)
	}
	if got, err := DeleteConnectionSecrets("mail", nil); !errors.Is(err, ErrVaultLocked) || got != nil {
		t.Errorf("DeleteConnectionSecrets() = %v, %v; want nothing deleted and ErrVaultLocked", got, err)
	}
}
