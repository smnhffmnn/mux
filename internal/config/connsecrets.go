package config

import (
	"errors"
	"strings"

	"github.com/zalando/go-keyring"
)

// connectionSecretSuffixes are the secret-store keys a connection can hold,
// as suffixes of its name — the same list ValidSecretKey accepts: password
// and token, the OAuth tokens and the client of a dynamically registered
// OAuth flow, the ssh key and its passphrase.
var connectionSecretSuffixes = []string{
	"-password", "-token",
	"-oauth-token", "-oauth-refresh-token",
	"-oauth-client-id", "-oauth-client-secret",
	"-private-key", "-private-key-passphrase",
}

// reservedSecretPrefixes are key namespaces of other owners — tunnels, the
// vault's own settings, provisioning endpoints. A connection named like
// "provisioning" or "tunnel-x" shares their spelling; deleting it never
// touches them.
var reservedSecretPrefixes = []string{"tunnel-", "vault-", "provisioning-"}

// ErrVaultLocked is returned when a connection's secrets cannot be removed
// because the vault holding them is locked.
var ErrVaultLocked = errors.New("the vault is locked — unlock it first, so the connection's secrets in it are deleted with the connection")

// ConnectionSecretsDeletable reports whether DeleteConnectionSecrets can
// reach every store. A locked vault cannot delete entries, and in exclusive
// mode it cannot even be checked; deleting the connection anyway would leave
// its secrets for the next connection of that name.
func ConnectionSecretsDeletable() error {
	if v := getActiveVault(); v != nil && !v.IsUnlocked() {
		return ErrVaultLocked
	}
	return nil
}

// DeleteConnectionSecrets removes every credential stored for the
// connection name and returns the keys it deleted. A credential must not
// outlive the connection it was stored for: kept, it would be loaded again
// by a connection added later under the same name — with another host, URL
// or allow_send — and sent wherever that one points. Kept are keys that are
// also a key of a remaining connection (the token of "foo-oauth" is spelled
// like the OAuth token of "foo") and keys in the reserved namespaces.
func DeleteConnectionSecrets(name string, remaining []Connection) ([]string, error) {
	if err := ConnectionSecretsDeletable(); err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, c := range remaining {
		for _, suffix := range connectionSecretSuffixes {
			kept[c.Name+suffix] = true
		}
	}
	var deleted []string
	for _, suffix := range connectionSecretSuffixes {
		key := name + suffix
		if kept[key] || reservedSecretKey(key) || !secretStored(key) {
			continue
		}
		if err := deleteSecret(key); err == nil {
			deleted = append(deleted, key)
		}
	}
	return deleted, nil
}

func reservedSecretKey(key string) bool {
	for _, p := range reservedSecretPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// secretStored reports whether any store holds key, without the resolution
// log lines getSecret writes.
func secretStored(key string) bool {
	if v := getActiveVault(); v != nil && v.IsUnlocked() {
		if val, err := v.GetSecret(key); err == nil && val != "" {
			return true
		}
	}
	if !skipKeyring.Load() {
		if val, err := keyring.Get(ServiceName, key); err == nil && val != "" {
			return true
		}
	}
	val, err := fileGet(key)
	return err == nil && val != ""
}
