# Deployment

## Prerequisites

- **Go 1.25+** (version from `go.mod`)
- **CGO** (only for macOS desktop build with system tray)
- **goreleaser** (optional, for release builds)

## Build Variants

mux has two build modes:

| Mode | Tray | CGO | Cross-compile | Use case |
|------|------|-----|---------------|----------|
| Desktop | Yes | Required | No (current platform only) | macOS developer workstation |
| Headless | No | Not needed | Yes (all platforms) | Servers, containers, CI, any OS |

The `notray` build tag disables the system tray, removing the CGO dependency on `fyne.io/systray`.

## Local Builds

### Desktop (macOS with system tray)

```bash
make build
# or:
go build -ldflags "-s -w -X main.version=dev" -o mux .
```

Requires CGO (enabled by default on macOS). Produces a binary with system tray support.

### Headless (any platform)

```bash
make build-headless
# or:
CGO_ENABLED=0 go build -tags notray -ldflags "-s -w -X main.version=dev" -o mux .
```

No CGO, no system tray. The web UI at `/ui` still works -- only the OS tray icon is removed.

### Cross-compile all platforms

```bash
make build-all
```

Produces 5 binaries in `dist/`:

| Binary | OS | Architecture |
|--------|----|-------------|
| `mux-darwin-arm64` | macOS | Apple Silicon |
| `mux-darwin-amd64` | macOS | Intel |
| `mux-linux-amd64` | Linux | x86_64 |
| `mux-linux-arm64` | Linux | ARM64 |
| `mux-windows-amd64.exe` | Windows | x86_64 |

All cross-compiled binaries are headless (`-tags notray`, `CGO_ENABLED=0`).

## Running

```bash
# HTTP mode (default when run from terminal)
mux --http

# Stdio mode (for Claude Desktop, piped agents)
mux --stdio

# Custom port
mux --http --port 8080

# Custom config file
mux --config /path/to/config.toml

# No system tray (desktop build only)
mux --http --no-tray

# No web UI
mux --http --no-ui
```

### Auto-detection

If no `--stdio` or `--http` flag is given, mux detects the mode automatically:
- stdin is a pipe -> stdio mode
- stdin is a terminal -> HTTP mode

## OS-Specific Notes

### macOS

**Desktop build** (recommended for development):
- System tray icon shows connection status
- Click tray icon to open web UI, see status, or quit
- Requires CGO (Xcode Command Line Tools: `xcode-select --install`)
- Gatekeeper may block unsigned binaries -- right-click -> Open, or `xattr -cr ./mux`

**Headless build**:
- Works without Xcode or CGO
- No tray icon, but web UI at `http://localhost:7700/ui` works fine
- Secrets stored in macOS Keychain Access (service: "mux")

### Linux

Only headless builds are supported (no system tray on Linux).

```bash
# Build
CGO_ENABLED=0 go build -tags notray -o mux .

# Run
./mux --http
```

**Keychain**: Secrets are stored via the Secret Service API (GNOME Keyring or KWallet). Ensure one of these is running:
- GNOME: `gnome-keyring-daemon` (usually started automatically in desktop sessions)
- KDE: KWallet

**For headless Linux servers** (no desktop environment): Use environment variables instead of the keychain for secrets:

```bash
export POSTGRESQL_DB_PASSWORD=secret
export MUX_ERP_TOKEN=my-erp-token
./mux --http
```

**systemd service** example:

```ini
[Unit]
Description=mux - MCP Unified Exchange
After=network.target

[Service]
Type=simple
User=mux
ExecStart=/usr/local/bin/mux --http --no-tray
Restart=on-failure
RestartSec=5
Environment=MUX_PORT=7700
# Add secrets as environment variables if no keyring is available:
# EnvironmentFile=/etc/mux/secrets.env

[Install]
WantedBy=multi-user.target
```

### Windows

Only headless builds are supported.

```powershell
# Build (from PowerShell)
$env:CGO_ENABLED=0; go build -tags notray -o mux.exe .

# Run
.\mux.exe --http
```

**Keychain**: Secrets are stored in Windows Credential Manager (service: "mux").

To run as a Windows Service, use a wrapper like [NSSM](https://nssm.cc/) or [WinSW](https://github.com/winsw/winsw).

### Docker

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags notray -ldflags "-s -w" -o mux .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/mux /usr/local/bin/mux
EXPOSE 7700
ENTRYPOINT ["mux", "--http", "--no-tray"]
```

Docker containers have no OS keychain. Use environment variables for all secrets:

```bash
docker run -p 7700:7700 \
  -e POSTGRESQL_DB_HOST=db.example.com \
  -e POSTGRESQL_DB_PASSWORD=secret \
  -e MUX_ERP_ENDPOINT=https://erp.example.com/api/mux/config \
  -e MUX_ERP_TOKEN=my-token \
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

### CI (`.github/workflows/ci.yml`)

Runs on every push to `main` and all pull requests:

1. **Test**: `go test -tags notray ./...`
2. **Build**: Headless binary for Linux
3. **Smoke test**: Sends an MCP `initialize` request via stdio and checks for `serverInfo` in the response
4. **Build matrix**: Verifies cross-compilation for all 5 platform/arch combinations

### Release (`.github/workflows/release.yml`)

Triggered by pushing a version tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Two jobs run:

1. **GoReleaser**: Creates a GitHub Release with headless binaries for all platforms (Linux, macOS, Windows x amd64/arm64)
2. **macOS Desktop**: Builds native macOS binaries with system tray on real macOS runners (Apple Silicon + Intel), then uploads to the same release

### Release artifacts

After a release, the GitHub Release page contains:

| File | Description |
|------|-------------|
| `mux_X.Y.Z_linux_amd64.tar.gz` | Linux x86_64 (headless) |
| `mux_X.Y.Z_linux_arm64.tar.gz` | Linux ARM64 (headless) |
| `mux_X.Y.Z_darwin_amd64.tar.gz` | macOS Intel (headless) |
| `mux_X.Y.Z_darwin_arm64.tar.gz` | macOS Apple Silicon (headless) |
| `mux_X.Y.Z_windows_amd64.zip` | Windows x86_64 (headless) |
| `mux_X.Y.Z_darwin_amd64_desktop.tar.gz` | macOS Intel (with system tray) |
| `mux_X.Y.Z_darwin_arm64_desktop.tar.gz` | macOS Apple Silicon (with system tray) |
| `checksums.txt` | SHA-256 checksums for all archives |

### GoReleaser snapshot

Test the release process locally without publishing:

```bash
make release-snapshot
# or:
goreleaser release --snapshot --clean
```

## Build Tags

| Tag | Effect |
|-----|--------|
| `notray` | Disables system tray (removes CGO dependency on fyne.io/systray) |
| *(none)* | Desktop build with system tray support |

## Version Embedding

The version is injected at build time via `-X main.version=...`:

```bash
# From Makefile
go build -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" -o mux .

# Check version
./mux --version
# mux v1.0.0
```

GoReleaser automatically uses the git tag as the version.
