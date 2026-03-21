# Deployment

## Prerequisites

- **Go 1.25+** (version from `go.mod`)
- **CGO** (only for macOS desktop build with system tray)

## Build Variants

| Mode | System Tray | CGO | Cross-compile | Use Case |
|------|-------------|-----|---------------|----------|
| Desktop | Yes | Required | No (macOS only) | Developer workstation |
| Headless | No | Not needed | Yes (all platforms) | Servers, containers, CI |

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

Or via `go install`:

```bash
go install github.com/smnhffmnn/mux@latest
```

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

- Headless binaries: Linux, macOS, Windows (amd64 + arm64)
- Desktop binaries: macOS with system tray (Apple Silicon + Intel)
- SHA-256 checksums

## Homebrew (macOS)

```bash
brew tap smnhffmnn/tap
brew install mux
```

## Running

```bash
# Auto-detect mode (terminal = HTTP, pipe = stdio)
mux

# Force HTTP mode
mux --http

# Force stdio mode
mux --stdio

# Custom port and config
mux --http --port 8080 --config /path/to/config.toml

# Disable system tray (desktop build only)
mux --http --no-tray

# Disable web UI
mux --http --no-ui

# Version
mux --version
```

### Mode Auto-Detection

If no `--stdio` or `--http` flag is given:
- stdin is a pipe -> stdio mode (for Claude Desktop, piped agents)
- stdin is a terminal -> HTTP mode + desktop app

## Platform Notes

### macOS

**Desktop build** (recommended for development):
- System tray icon shows connection status
- Click tray icon to open web UI, see status, or quit
- Requires CGO (Xcode Command Line Tools)
- Gatekeeper may block unsigned binaries -- right-click -> Open, or `xattr -cr ./mux`

**Headless build**:
- Works without Xcode
- No tray icon, but web UI at `http://localhost:7700/ui` still works
- Secrets stored in macOS Keychain Access (service: "mux")

### Linux

Only headless builds are supported.

```bash
CGO_ENABLED=0 go build -tags notray -o mux .
./mux --http
```

**Keychain**: Secrets use the Secret Service API (GNOME Keyring or KWallet). For headless servers without a desktop environment, use environment variables instead:

```bash
export POSTGRESQL_DB_PASSWORD=secret
export MUX_PROVISIONING_TOKEN=my-token
./mux --http
```

### Windows

Only headless builds are supported.

```powershell
$env:CGO_ENABLED=0; go build -tags notray -o mux.exe .
.\mux.exe --http
```

Secrets are stored in Windows Credential Manager (service: "mux").

## Running as a Service

### systemd (Linux)

```ini
[Unit]
Description=mux MCP gateway
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

### Windows Service

Use a service wrapper like [NSSM](https://nssm.cc/) or [WinSW](https://github.com/winsw/winsw):

```bash
nssm install mux "C:\path\to\mux.exe" "--http --no-tray"
nssm start mux
```

## Docker

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

GoReleaser creates a GitHub Release with headless binaries for all platforms. A separate macOS job builds desktop binaries with system tray support.

## Version Embedding

The version is injected at build time via ldflags:

```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" -o mux .
./mux --version
```

GoReleaser uses the git tag automatically.
