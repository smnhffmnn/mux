package config

// connectionSecretSuffixes are the secret-store keys a connection can hold,
// as suffixes of its name: password and token (every type that has them),
// the ssh key and its passphrase, the OAuth tokens of proxy and device-code
// flows.
var connectionSecretSuffixes = []string{
	"-password",
	"-token",
	"-private-key",
	"-private-key-passphrase",
	"-oauth-token",
	"-oauth-refresh-token",
}

// DeleteConnectionSecrets removes every credential stored for the
// connection name and returns the keys that existed. A credential must not
// outlive the connection it was stored for: kept, it would be loaded again
// by a connection added later under the same name — with another host, URL
// or allow_send — and sent wherever that one points. Keys that are also a
// key of a remaining connection stay (the token of "foo-oauth" is spelled
// like the OAuth token of "foo").
func DeleteConnectionSecrets(name string, remaining []Connection) []string {
	kept := map[string]bool{}
	for _, c := range remaining {
		for _, suffix := range connectionSecretSuffixes {
			kept[c.Name+suffix] = true
		}
	}
	var deleted []string
	for _, suffix := range connectionSecretSuffixes {
		key := name + suffix
		if kept[key] {
			continue
		}
		if v, err := getSecret(key); err != nil || v == "" {
			continue
		}
		if err := deleteSecret(key); err == nil {
			deleted = append(deleted, key)
		}
	}
	return deleted
}
