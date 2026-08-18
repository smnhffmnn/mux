package config

import "testing"

// useFileSecretStore points the secret store at a throwaway directory and
// disables the OS keyring for the test, so SaveSecret/getSecret run purely
// against secrets.toml.
func useFileSecretStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	prevSkip := skipKeyring.Load()
	skipKeyring.Store(true)
	t.Cleanup(func() { skipKeyring.Store(prevSkip) })
}

func TestResolveConnectionSecretsFillsEmptyFields(t *testing.T) {
	useFileSecretStore(t)

	if err := SaveSecret("mydb-password", "pw-from-store"); err != nil {
		t.Fatalf("SaveSecret password: %v", err)
	}
	if err := SaveSecret("mydb-token", "token-from-store"); err != nil {
		t.Fatalf("SaveSecret token: %v", err)
	}

	conn := Connection{Name: "mydb"}
	if got := ResolveConnectionSecrets(&conn); got != 2 {
		t.Fatalf("ResolveConnectionSecrets filled %d fields, want 2", got)
	}
	if conn.Password != "pw-from-store" {
		t.Errorf("Password = %q, want %q", conn.Password, "pw-from-store")
	}
	if conn.Token != "token-from-store" {
		t.Errorf("Token = %q, want %q", conn.Token, "token-from-store")
	}
}

func TestResolveConnectionSecretsKeepsExistingValues(t *testing.T) {
	useFileSecretStore(t)

	if err := SaveSecret("mydb-password", "stale-store-copy"); err != nil {
		t.Fatalf("SaveSecret password: %v", err)
	}
	if err := SaveSecret("mydb-token", "stale-store-copy"); err != nil {
		t.Fatalf("SaveSecret token: %v", err)
	}

	conn := Connection{Name: "mydb", Password: "just-typed", Token: "provisioned"}
	if got := ResolveConnectionSecrets(&conn); got != 0 {
		t.Fatalf("ResolveConnectionSecrets filled %d fields, want 0 (both already set)", got)
	}
	if conn.Password != "just-typed" {
		t.Errorf("Password = %q, want the in-memory value %q", conn.Password, "just-typed")
	}
	if conn.Token != "provisioned" {
		t.Errorf("Token = %q, want the in-memory value %q", conn.Token, "provisioned")
	}
}

func TestResolveConnectionSecretsNoSecretsStored(t *testing.T) {
	useFileSecretStore(t)

	conn := Connection{Name: "mydb"}
	if got := ResolveConnectionSecrets(&conn); got != 0 {
		t.Fatalf("ResolveConnectionSecrets filled %d fields, want 0 (store is empty)", got)
	}
	if conn.Password != "" || conn.Token != "" {
		t.Errorf("fields were mutated although the store is empty: password=%q token=%q", conn.Password, conn.Token)
	}
}
