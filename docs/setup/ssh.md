# SSH (commands & files)

## Prerequisites

- An account on the host that accepts public-key authentication.
- The host's SFTP subsystem enabled (default in OpenSSH) for `upload` and `download`; `exec` works without it.
- The host's key known to mux: either pinned in **Host Key**, or present in `known_hosts` in the mux config directory or in `~/.ssh/known_hosts`. The first connection attempt to an unknown host fails with the exact `known_hosts` line to add; the files are re-read on the next attempt.

## Fields

| Field | Description |
|------|-------------|
| Host | Hostname or IP of the server. |
| Port | Default: `22`. |
| User | Login on the host. |
| Auth | `key` (default): mux signs with a private key it holds — the one pasted below, or **Key File**. `agent`: mux asks the ssh-agent of the user it runs as (`SSH_AUTH_SOCK`); no key is stored in mux. Agents are not available in containers or on Windows. |
| Private Key (PEM, optional) | The private key, pasted as a whole (OpenSSH or PKCS format). May be passphrase-protected. Stored as secret `{name}-private-key`. Not needed with Key File or `auth = agent`. |
| Key Passphrase (optional) | Decrypts the private key when it is encrypted. Stored as secret `{name}-private-key-passphrase`. Ignored for an unencrypted key. |
| Key File (optional) | Path of a private key file to read instead of the stored key (`~` allowed). |
| Host Key (optional) | The server's public key (`ssh-ed25519 AAAA…`) to pin. Empty: the `known_hosts` files decide. There is no way to skip verification. |
| Allowed commands (optional) | One per line, as leading tokens: `systemctl status`, `docker logs`. The program is matched as written (`cat` does not cover `/bin/cat`; list the path if it may run). On a read-only connection an empty list means the built-in list of reading commands. |
| Denied commands (optional) | One per line, same form, wildcards `*` and `?` allowed (`journalctl --vacuum*`). Always wins over allowed commands; with a deny list present, `shell=true` is refused. |

`read_only = true` (config.toml or `connection_add`) removes `upload`, refuses `shell=true` and limits `exec` to the allowed commands.

## After creating

Click **Test**. A successful test reports the server's SSH version, the key or agent used, and how the host key was verified. An unknown host key fails the test and shows the line to paste into `known_hosts`.

## Notes

- `exec` sends one program with arguments. Every argument is quoted for the remote shell, so no globbing, variable expansion or pipes happen unless `shell=true` is passed — which is only possible on a connection without allowed or denied commands and without `read_only`.
- `dry_run=true` on `exec` shows the exact command line and the policy verdict without connecting.
- The allow and deny lists express what the connection is for. They are enforced in code for every client, but they are not a sandbox: `cat` as root reads every file, and `tail -f` runs until the timeout.
- Every command and file copy is written to the mux log and to `GET /audit/ssh` on the loopback listener (`http://127.0.0.1:7700/audit/ssh`, newest first, `?limit=` up to 500). Arguments are logged as sent, so a secret passed as an argument lands there too.
