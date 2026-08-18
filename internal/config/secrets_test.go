package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// stubKeyring replaces the keyring indirection for one test and restores it
// afterwards, together with the skipKeyring marker. HOME/XDG are pointed at
// throwaway dirs so the file store never touches a real secrets.toml.
func stubKeyring(t *testing.T, set func(service, key, value string) error, get func(service, key string) (string, error), del func(service, key string) error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	prevSet, prevGet, prevDel := keyringSet, keyringGet, keyringDelete
	prevSkip := skipKeyring.Load()
	keyringSet, keyringGet, keyringDelete = set, get, del
	skipKeyring.Store(false)
	t.Cleanup(func() {
		keyringSet, keyringGet, keyringDelete = prevSet, prevGet, prevDel
		skipKeyring.Store(prevSkip)
	})
}

var errKeyringDenied = errors.New("keyring: access denied")

func TestSetSecretRemovesFossilOnFailedWrite(t *testing.T) {
	deleted := false
	stubKeyring(t,
		func(_, _, _ string) error { return errKeyringDenied },
		func(_, _ string) (string, error) {
			t.Error("unexpected keyring read — fossil was already removed")
			return "", keyring.ErrNotFound
		},
		func(_, _ string) error { deleted = true; return nil },
	)

	if err := SaveSecret("db-password", "fresh"); err != nil {
		t.Fatalf("SaveSecret returned %v, want nil (fossil was removed)", err)
	}
	if !deleted {
		t.Fatal("stale keyring entry was not deleted after failed write")
	}
	if v, err := fileGet("db-password"); err != nil || v != "fresh" {
		t.Fatalf("file store holds (%q, %v), want (%q, nil)", v, err, "fresh")
	}
	if !skipKeyring.Load() {
		t.Fatal("systemic keyring failure did not set skipKeyring")
	}
}

func TestSetSecretSurfacesSurvivingFossil(t *testing.T) {
	stubKeyring(t,
		func(_, _, _ string) error { return errKeyringDenied },
		func(_, _ string) (string, error) { return "stale", nil },
		func(_, _ string) error { return errKeyringDenied },
	)

	err := SaveSecret("db-password", "fresh")
	if err == nil {
		t.Fatal("SaveSecret returned nil although a readable stale keyring entry survives")
	}
	if !strings.Contains(err.Error(), "db-password") {
		t.Errorf("error should name the key, got: %v", err)
	}
	// The fresh value must be persisted to the file store regardless.
	if v, ferr := fileGet("db-password"); ferr != nil || v != "fresh" {
		t.Fatalf("file store holds (%q, %v), want (%q, nil)", v, ferr, "fresh")
	}
}

func TestSetSecretHeadlessKeyringFailureStaysSilent(t *testing.T) {
	sysErr := errors.New("dbus: no session bus")
	stubKeyring(t,
		func(_, _, _ string) error { return sysErr },
		func(_, _ string) (string, error) { return "", sysErr },
		func(_, _ string) error { return sysErr },
	)

	if err := SaveSecret("api-token", "fresh"); err != nil {
		t.Fatalf("SaveSecret returned %v, want nil (keyring unusable, file fallback is the designed path)", err)
	}
	if v, err := fileGet("api-token"); err != nil || v != "fresh" {
		t.Fatalf("file store holds (%q, %v), want (%q, nil)", v, err, "fresh")
	}
}

func TestSetSecretNoFossilToRemove(t *testing.T) {
	stubKeyring(t,
		func(_, _, _ string) error { return errKeyringDenied },
		func(_, _ string) (string, error) {
			t.Error("unexpected keyring read — delete already reported not-found")
			return "", keyring.ErrNotFound
		},
		func(_, _ string) error { return keyring.ErrNotFound },
	)

	if err := SaveSecret("api-token", "fresh"); err != nil {
		t.Fatalf("SaveSecret returned %v, want nil (no stale entry existed)", err)
	}
}

func TestSetSecretKeyringHealthyWritesBothStores(t *testing.T) {
	stored := map[string]string{}
	stubKeyring(t,
		func(_, key, value string) error { stored[key] = value; return nil },
		func(_, key string) (string, error) {
			v, ok := stored[key]
			if !ok {
				return "", keyring.ErrNotFound
			}
			return v, nil
		},
		func(_, key string) error { delete(stored, key); return nil },
	)

	if err := SaveSecret("db-password", "fresh"); err != nil {
		t.Fatalf("SaveSecret returned %v, want nil", err)
	}
	if stored["db-password"] != "fresh" {
		t.Fatalf("keyring holds %q, want %q", stored["db-password"], "fresh")
	}
	if v, err := fileGet("db-password"); err != nil || v != "fresh" {
		t.Fatalf("file store holds (%q, %v), want (%q, nil)", v, err, "fresh")
	}
	if skipKeyring.Load() {
		t.Fatal("healthy keyring write must not set skipKeyring")
	}
}
