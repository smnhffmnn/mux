# Security Policy

## Reporting Vulnerabilities

If you discover a security vulnerability in mux, **please do not open a public issue.**

Instead, use [GitHub's private vulnerability reporting](https://github.com/smnhffmnn/mux/security/advisories/new) to submit a report. This allows coordinated discussion and fix preparation before public disclosure.

## Scope

Security reports are welcome for:
- The vault encryption system (AES-256-GCM, Argon2id, key management)
- WebAuthn/FIDO2 authentication
- Secret storage and retrieval (keychain, file store, vault)
- HTTP/HTTPS server security (TLS config, headers, CORS)
- MCP tool authorization and input validation
- Tunnel security (WireGuard, SSH)
- OAuth token handling

## Security Model

mux is designed as a **local MCP gateway** running on a developer's machine or a trusted server. The security model assumes:

- **Trusted local user**: Processes running as the same OS user can access the mux config directory (`~/.mux/`). The vault protects secrets at the API boundary (runtime), not against local disk access by the same user.
- **`vault.key`**: When WebAuthn is configured, an auth key file (`~/.mux/vault.key`) enables passwordless vault unlock. This file is equivalent to the master passphrase — protect it with filesystem permissions.
- **`secrets.toml`**: When `exclusive` mode is **off** (the default), secrets are written to plaintext `~/.mux/secrets.toml` alongside the encrypted vault. Set `exclusive = true` in your vault config to prevent this.
- **Network exposure**: The HTTPS server (for WebAuthn/Vault) binds to all interfaces. The MCP endpoint is localhost-only. TLS 1.2+ is enforced on the HTTPS listener.

## Supported Versions

Only the latest release is supported with security updates.
