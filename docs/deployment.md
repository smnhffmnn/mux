# Deployment

## Prerequisites

- **Go 1.25+** (version from `go.mod`)
- **CGO** (required for macOS and Windows desktop builds with system tray/GUI)

## Build Variants

| Mode | System Tray | CGO | Platforms | Use Case |
|------|-------------|-----|-----------|----------|
| Desktop | Yes | Required | macOS, Windows | Developer workstation |
| Headless | No | Not needed | All (cross-compilable) | Servers, containers, CI |

The `notray` build tag disables the system tray, removing the CGO dependency.

## Building from Source

### Desktop (macOS with system tray)

```bash
git clone https://github.com/smnhffmnn/mux.git
cd mux
make build
```

Requires Xcode Command Line Tools (`xcode-select --install`) for CGO.

### Headless (any platform)

```bash
CGO_ENABLED=0 go build -tags notray -ldflags "-s -w" -o mux .
```

> **Note:** `go install` is not recommended for headless builds — it cannot pass the `-tags notray` flag and will attempt to build the desktop variant, which requires CGO.

### Cross-compile

```bash
# Linux amd64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags notray -o mux-linux-amd64 .

# Linux arm64
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags notray -o mux-linux-arm64 .

# Windows amd64
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags notray -o mux-windows-amd64.exe .
```

## GitHub Releases

Pre-built binaries for all platforms are available on the [Releases page](https://github.com/smnhffmnn/mux/releases). Each release includes:

- Headless binaries: Linux (amd64 + arm64)
- Desktop binaries: macOS (Apple Silicon + Intel), Windows (amd64)
- SHA-256 checksums

## Homebrew (macOS)

```bash
brew tap smnhffmnn/tap
brew install mux
```

## Running

```bash
# Auto-detect mode (see below)
mux

# Custom port and config
mux --port 8080 --config /path/to/config.toml

# Version
mux --version
```

### Mode Auto-Detection

mux automatically selects the right mode — no flags needed:

1. **stdin is a pipe** → stdio mode (for Claude Desktop, piped agents)
   - If another instance is already serving MCP on the configured port, the stdio invocation bridges to it instead of starting its own tunnels and vault (`[server] stdio_proxy`, see [Running more than one instance](configuration.md#running-more-than-one-instance))
2. **No DISPLAY/WAYLAND_DISPLAY** (Linux only) → headless HTTP mode (servers, containers)
3. **Otherwise** → desktop GUI mode (macOS, Windows, Linux with display)

## Platform Notes

### macOS

**Desktop build** (recommended for development):
- System tray icon shows connection status
- Click tray icon to open web UI, see status, or quit
- Requires CGO (Xcode Command Line Tools)
- Gatekeeper may block unsigned binaries -- right-click -> Open, or `xattr -cr ./mux`

**Headless build**:
- Works without Xcode
- No tray icon — MCP endpoint only (`http://localhost:7700/mcp`)
- Secrets stored in macOS Keychain Access (service: "mux"), or in the encrypted vault

### Linux

Only headless builds are supported.

```bash
CGO_ENABLED=0 go build -tags notray -o mux .
./mux
```

mux detects the absence of `DISPLAY`/`WAYLAND_DISPLAY` and starts in headless HTTP mode automatically.

**Secrets**: On Linux with a desktop environment, secrets use the Secret Service API (GNOME Keyring or KWallet). For headless servers, the recommended options are:

1. **Encrypted Vault** (recommended) -- hardware-grade encryption with WebAuthn unlock. See [Configuration: Vault](configuration.md#vault-encrypted-secret-store).
2. **File fallback** -- `secrets.toml` next to the config file (e.g. `~/.config/mux/secrets.toml`, chmod 600), used automatically when no keyring is available.
3. **Environment variables** -- override connection settings for containerized deployments:
   ```bash
   export POSTGRESQL_DB_PASSWORD=secret
   export MUX_PROVISIONING_TOKEN=my-token
   ./mux
   ```

### Windows

**Desktop build** (recommended):

1. Download `mux_<version>_windows_amd64.zip` from the [Releases page](https://github.com/smnhffmnn/mux/releases)
2. Extract the ZIP to a folder (e.g. `C:\Users\<you>\mux\`)
3. Run `mux.exe` -- the desktop GUI opens with system tray icon
4. Click the tray icon to open the web UI, manage connections, and configure settings
5. Config file: `%USERPROFILE%\.mux\config.toml` (created on first run)

**Requirements:**
- Windows 10/11 (amd64)
- [Microsoft WebView2 Runtime](https://developer.microsoft.com/en-us/microsoft-edge/webview2/) (pre-installed on Windows 11, may need install on Windows 10)

**Secrets** are stored in Windows Credential Manager (service: "mux").

**SmartScreen:** The binary is not code-signed yet. Windows SmartScreen may show "Windows protected your PC" on first launch. Click "More info" → "Run anyway".

**Self-update:** mux can update itself -- it downloads the latest `mux.exe` from GitHub Releases when a new version is available.

**Claude Desktop integration:** Add to `%APPDATA%\Claude\claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "mux": {
      "command": "C:\\Users\\<you>\\mux\\mux.exe"
    }
  }
}
```

When launched via Claude Desktop, mux detects the piped stdin and runs in stdio mode (no GUI window).

## Running as a Service

### systemd (Linux)

```ini
[Unit]
Description=mux MCP gateway
After=network.target

[Service]
Type=simple
User=mux
Group=mux
ExecStart=/usr/local/bin/mux
Restart=on-failure
RestartSec=5
# Secrets: use the encrypted vault (recommended), or set environment overrides:
# EnvironmentFile=/etc/mux/secrets.env

[Install]
WantedBy=multi-user.target
```

For user-level services (recommended for workstation setups with vault + WebAuthn):

```ini
# ~/.config/systemd/user/mux.service
[Unit]
Description=mux MCP gateway
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/mux
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Enable with `systemctl --user enable --now mux.service`. Requires `loginctl enable-linger <user>` for the service to survive logout.

### Windows Service

Use a service wrapper like [NSSM](https://nssm.cc/) or [WinSW](https://github.com/winsw/winsw):

```bash
nssm install mux "C:\path\to\mux.exe"
nssm start mux
```

## Docker

```dockerfile
FROM golang:1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags notray -ldflags "-s -w" -o mux .

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/mux /usr/local/bin/mux
EXPOSE 7700
ENTRYPOINT ["mux"]
```

Docker containers have no OS keychain or vault. Use environment variables to override connection settings:

```bash
docker run -p 7700:7700 \
  -e POSTGRESQL_DB_HOST=db.example.com \
  -e POSTGRESQL_DB_PASSWORD=secret \
  -e MUX_PROVISIONING_ENDPOINT=https://provisioning.example.com/api/mux/config \
  -e MUX_PROVISIONING_TOKEN=my-token \
  mux
```

Or mount a config file:

```bash
docker run -p 7700:7700 \
  -v /path/to/config.toml:/root/.mux/config.toml:ro \
  -e POSTGRESQL_DB_PASSWORD=secret \
  mux
```

## CI/CD

### CI

Runs on every push to `main` and all pull requests:

1. `go test -tags notray ./...`
2. Headless build for Linux
3. Smoke test: sends an MCP `initialize` request via stdio and checks for `serverInfo`
4. Build matrix: verifies cross-compilation for all platform/arch combinations

### Release

Triggered by pushing a version tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The release workflow creates a GitHub Release with:
- Desktop binaries for macOS (amd64 + arm64) and Windows (amd64)
- Headless binaries for Linux (amd64 + arm64)

## Version Embedding

The version is injected at build time via ldflags:

```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" -o mux .
./mux --version
```

GoReleaser uses the git tag automatically.
