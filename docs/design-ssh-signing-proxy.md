# Design: SSH Signing Proxy via Vault

**Status:** Draft
**Author:** Simon Hoffmann / Claude
**Created:** 2026-04-01
**Last Updated:** 2026-04-01

## Problem

Autonomous Claude Code sessions (via Harness) need to perform git push operations on behalf of the user. These operations require SSH authentication to GitLab/GitHub. The current situation:

1. **FIDO2 SSH keys (`ed25519-sk`)** require the physical YubiKey plugged into the PC's USB port. They cannot be triggered remotely. This makes them unusable for the "approve from phone" use case.

2. **Regular SSH keys with passphrase** require the passphrase to be entered after each reboot to load the key into `ssh-agent`. Without a human at the keyboard, the agent is empty after reboot.

3. **Regular SSH keys without passphrase** work after reboot, but any process running as the same user can use them freely — no authorization boundary.

The goal is: **SSH operations should be possible only after explicit remote approval via WebAuthn (FaceID/YubiKey NFC), on a per-action basis.**

## Goals

- SSH private key encrypted at rest (not readable on disk without vault unlock)
- Remote approval per SSH operation (git push, SSH to server) via existing WebAuthn/Telegram flow
- Key only available in ssh-agent for the duration of the approved operation
- Works after reboot without human physically at the PC
- Integrates with existing mux vault, approval queue, and Harness hook system

## Non-Goals

- Replacing SSH entirely (SSH remains the transport)
- Supporting multiple SSH keys for different services (V1: one key for everything)
- Acting as a full SSH Certificate Authority (that's a future option)
- Browser-based SSH client (the operations run on the PC via Harness)

## Current Architecture

```
Harness (Telegram Bot)
  │
  ├── PreToolUse Hook detects "git push"
  ├── Creates Approval Request at mux /vault/approval
  ├── mux sends Telegram notification → user approves via WebAuthn
  ├── Hook polls until granted
  └── Allows the Bash tool → git push runs
        │
        └── git push → SSH → needs key in ssh-agent → ??? (broken)
```

The approval flow works. The problem is the last mile: the SSH key isn't available after approval.

## Proposed Solution

### Overview

mux stores the SSH private key as a vault secret. When an SSH operation is approved, mux temporarily injects the key into `ssh-agent`, waits for the operation to complete, then removes it.

```
Harness detects "git push"
  │
  ├── POST /vault/approval → Telegram notification
  ├── User approves via WebAuthn (FaceID on iPhone)
  ├── POST /vault/ssh/load → mux loads key into ssh-agent
  │     ├── Reads key from vault (must be unlocked)
  │     ├── ssh-add with configurable lifetime (e.g. 60s)
  │     └── Returns success
  ├── git push runs → uses key from ssh-agent → succeeds
  └── Key auto-expires from agent after lifetime
```

### Key Storage

The SSH private key is stored as a regular vault secret:

```
secret_set key=ssh-private-key value="-----BEGIN OPENSSH PRIVATE KEY-----\n..."
```

Alternatively, mux can import from an existing key file:

```
POST /vault/ssh/import { "key_path": "~/.ssh/id_rsa", "passphrase": "..." }
```

This reads the key, decrypts it (if passphrase-protected), stores the raw private key in the vault, and optionally deletes the original file.

### Agent Injection

mux uses `ssh-add` (or the Go `x/crypto/ssh/agent` library) to add the key to the running ssh-agent:

```go
// Connect to SSH agent
sock := os.Getenv("SSH_AUTH_SOCK")
conn, _ := net.Dial("unix", sock)
agent := agent.NewClient(conn)

// Add key with lifetime (auto-removes after expiry)
agent.Add(agent.AddedKey{
    PrivateKey:   parsedKey,
    LifetimeSecs: 60, // key vanishes after 60 seconds
    Comment:      "mux-vault-temporary",
})
```

The `LifetimeSecs` parameter is critical: ssh-agent automatically removes the key after this duration. Even if mux crashes, the key expires.

### API

Two new endpoints on the vault HTTP API:

#### `POST /vault/ssh/load`

Loads the SSH key from vault into ssh-agent with a short lifetime.

**Request:** `{ "lifetime_secs": 60 }` (optional, default 60)
**Auth:** Vault must be unlocked
**Response:** `{ "status": "loaded", "expires_in": 60, "fingerprint": "SHA256:..." }`

#### `POST /vault/ssh/import`

Imports an existing SSH key file into the vault.

**Request:** `{ "key_path": "/path/to/key", "passphrase": "optional", "delete_original": false }`
**Auth:** Vault must be unlocked
**Response:** `{ "status": "imported", "fingerprint": "SHA256:...", "type": "ed25519" }`

#### `GET /vault/ssh/status`

Shows whether a key is loaded in the agent.

**Response:** `{ "stored": true, "loaded_in_agent": false, "fingerprint": "SHA256:..." }`

### MCP Tools

Two new MCP tools for use by Claude Code sessions:

- `vault_ssh_load` — Load key into agent (requires vault unlocked)
- `vault_ssh_status` — Check if key is in agent

### Harness Integration

The Harness approval flow changes slightly:

```typescript
// In session.ts, after approval is granted:
if (result.decision === "approval" && approved) {
  // Load SSH key into agent before allowing the push
  await fetch(`${MUX_URL}/vault/ssh/load`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ lifetime_secs: 120 }),
  });
  // Now allow the tool — git push will find the key in the agent
}
```

### Setup Flow (One-Time)

1. Generate a new ed25519 key (no passphrase): `ssh-keygen -t ed25519 -f /tmp/mux-ssh-key -N ""`
2. Register the public key in GitLab and GitHub
3. Import into vault: call `/vault/ssh/import` with the key path
4. Delete the original key file
5. Verify: unlock vault → load key → `ssh -T git@github.com`

After this, no SSH key exists on disk. The only copy is in the encrypted vault.

## Security Model

### Threat Analysis

| Threat | Mitigation |
|--------|-----------|
| Key on disk in plaintext | Key only in vault (encrypted AES-256-GCM). No file on disk after import. |
| Process reads key from memory | Key is in ssh-agent process memory, not mux. Agent is a separate process with its own address space. |
| Unauthorized use of loaded key | Key has 60s lifetime in agent. Auto-expires. Only loaded after WebAuthn approval. |
| Agent sock accessible to other processes | Same-user processes can use the agent sock. Mitigated by short lifetime (60s window). |
| Attacker loads key without approval | `/vault/ssh/load` requires vault unlocked. Vault unlock requires WebAuthn (physical authenticator). |
| Key persists after crash | `LifetimeSecs` on ssh-add. Even if mux crashes, agent removes the key after expiry. |
| Reboot | Vault sealed, key in encrypted vault only. Agent empty. No key accessible. |

### Trust Boundary

The security boundary is the **vault unlock** (WebAuthn). Everything before unlock: no SSH access possible. After unlock: key can be loaded on demand. The short lifetime (60s) limits the exposure window.

**Important:** This does NOT protect against a malicious process running as the same user that monitors the agent sock and uses the key during the 60s window. The protection is against:
- Unattended access (reboot, idle PC)
- Unauthorized remote access (no WebAuthn = no key)
- Disk forensics (key only exists encrypted)

### Comparison to Previous Approach

| Aspect | FIDO2 SSH Key | Vault SSH Proxy |
|--------|--------------|-----------------|
| Key on disk | Key handle (useless without YubiKey) | Encrypted in vault |
| Remote approval | Impossible (needs physical presence) | Yes (WebAuthn via phone) |
| After reboot | Broken (YubiKey not plugged in) | Works (vault unlock → load) |
| Per-operation | Yes (YubiKey touch per signature) | Yes (approval + 60s lifetime) |
| Attack surface | Hardware security element | Software (vault encryption + agent lifetime) |

The FIDO2 approach has stronger cryptographic guarantees (hardware element), but is unusable for the remote approval use case. The vault approach trades hardware-bound keys for remote operability, with software-enforced time limits.

## Implementation Plan

### Phase 1: Core (MVP)

1. **`internal/vault/ssh.go`** — SSH key storage + agent injection
   - Parse SSH private keys (ed25519, RSA, ECDSA)
   - Connect to ssh-agent via `SSH_AUTH_SOCK`
   - Add/remove key with lifetime
   - Fingerprint calculation
2. **`internal/vault/handler.go`** — HTTP endpoints (`/vault/ssh/load`, `/vault/ssh/import`, `/vault/ssh/status`)
3. **`internal/tools/vault.go`** — MCP tools (`vault_ssh_load`, `vault_ssh_status`)
4. **Tests** — Unit tests for key parsing, agent interaction
5. **Harness `session.ts`** — Call `/vault/ssh/load` after approval grant

### Phase 2: UX

6. **Manage page** — Show SSH key status, import UI, manual load button
7. **Auto-load on approval** — Harness automatically loads key when approving git push (no manual step)
8. **`ssh-agent` systemd unit** — Ensure agent is running after reboot (may already exist)

### Phase 3: Multi-Key (Future)

9. Support multiple SSH keys (per-repo or per-host)
10. Key rotation (generate new key, update GitLab/GitHub, migrate)

## Open Questions

1. **`SSH_AUTH_SOCK` availability:** Does the systemd user ssh-agent set `SSH_AUTH_SOCK` for all user services? mux needs to know the socket path. May need explicit configuration or env var.

2. **Key lifetime vs. long operations:** 60 seconds may not be enough for a large git push over slow connection. Should be configurable. Could also use "load, wait for operation, unload" instead of time-based expiry.

3. **Multiple concurrent operations:** If two sessions both need SSH at the same time, the key is loaded once and both can use it. Is this acceptable? (Probably yes — the approval gate is per-operation, the key-in-agent is just an implementation detail.)

4. **Git credential helper alternative:** Instead of ssh-agent, could mux act as a Git credential helper? This would avoid the agent sock question but only works for HTTPS Git, not SSH.

5. **Should the FIDO2 key be removed from GitLab/GitHub?** It's now redundant. Or keep it as a backup for when Simon is physically at the PC.

## Alternatives Considered

### A. SSH Certificate Authority

mux generates short-lived SSH certificates signed by a CA key. Each git push gets a fresh certificate valid for 60 seconds.

**Pros:** No key in agent, no lifetime management, each cert is unique.
**Cons:** Requires CA setup on GitLab/GitHub (complex), not supported by all Git hosting.
**Verdict:** Too complex for V1. Good future option.

### B. Git HTTPS + Token

Switch all repos from SSH to HTTPS and use Personal Access Tokens stored in the vault. Load tokens via Git credential helper.

**Pros:** No SSH complexity, tokens can be scoped per-repo.
**Cons:** Requires changing all remote URLs. GitLab and GitHub have different token formats. SSH is more universal.
**Verdict:** Viable alternative but disruptive.

### C. SSH Agent Forwarding from Phone

Run an SSH agent on the iPhone that holds the key, forward it to the PC via Tailscale.

**Pros:** Key never on the PC at all.
**Cons:** No iOS SSH agent app with this capability. Would need a custom app.
**Verdict:** Not feasible with current tools.

### D. Keep FIDO2 + Physical Presence

Accept that git push requires Simon to be at the PC with YubiKey.

**Pros:** Strongest security.
**Cons:** Defeats the entire Harness remote-control use case.
**Verdict:** Rejected — the point is remote operation.
