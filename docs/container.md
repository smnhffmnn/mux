# Running mux in a container

The image contains the binary and nothing else — no `config.toml`, no secrets,
no connection definitions. Everything arrives at runtime, so the same image
serves any number of unrelated instances.

```bash
docker build -t mux:dev .
docker run -d --name mux \
  -e MUX_PROVISIONING_ENDPOINT=https://example.com/api/mux/provision \
  -e MUX_PROVISIONING_TOKEN=... \
  mux:dev
```

## No network privileges

mux carries gvisor-netstack and wireguard-go and does WireGuard in userspace.
The container needs no `NET_ADMIN` and no `/dev/net/tun`, and runs as
`nonroot`.

## Configuration

Three ways in, in the order worth reaching for:

**Provisioning** — mux fetches its tunnels and connections from a provisioning
endpoint at startup. The container then holds exactly one secret, the
provisioning token, and pulls the rest itself:

```
MUX_PROVISIONING_ENDPOINT=https://example.com/api/mux/provision
MUX_PROVISIONING_TOKEN=...
```

**Mount** — for anything not delivered by provisioning, mount a config
directory at **`/config/mux`**. mux resolves its config directory as
`$XDG_CONFIG_HOME/mux`, and the image sets `XDG_CONFIG_HOME=/config`, so the
mount point maps one-to-one onto `~/.config/mux` on a host:

```bash
docker run -v ~/.config/mux:/config/mux mux:dev
```

Mounting `/config` instead fails silently: mux finds no `config.toml` there and
starts with defaults, logging nothing that points at the mount.

Secrets in a mounted `secrets.toml` work as they do on a host. There is no
keychain in the container — the keyring probe fails immediately
(`dbus-launch: executable file not found`) and mux uses the file store.

Mount it read-write. mux writes its log file there, and the config-management
MCP tools (`connection_add`, `secret_set`, …) write `config.toml` and
`secrets.toml`. A read-only mount does not stop the container from starting —
the provisioning path never writes — but those tools will fail.

**Environment** — individual settings and secrets (`MUX_PORT`, and the
per-connection variables listed in `config.example.toml`). Fine for single
values, unsuited to structured connection definitions.

There is no vault in the container. The vault unlocks interactively
(passphrase or WebAuthn) and nothing in a headless container can answer that
prompt; in exclusive mode a sealed vault refuses every secret read. Leave
`[vault] enabled` off here and let secrets come from provisioning or the
mounted `secrets.toml`.

OAuth connections cannot complete a login in the container either — the
headless build registers no `/oauth/*` routes. Connections that need OAuth are
reported by the health endpoint as `awaiting oauth login` and stay
unregistered.

## Health

`GET /health` answers the question a port check cannot: is this instance
serving what it was configured to serve?

The failure it exists for is silent. When mux starts before DNS is up — routine
for a container that boots alongside its network — the provisioning fetch
fails, mux logs it and carries on. The port answers, `/mcp` answers, and the
provisioned connections are simply missing.

```json
{
  "status": "degraded",
  "version": "0.0.0-dev",
  "uptimeSeconds": 6,
  "provisioning": [
    { "name": "default", "url": "https://…", "delivered": false, "tunnels": 0, "connections": 0 }
  ],
  "tunnels": [],
  "connections": [],
  "problems": ["provisioning endpoint \"default\" has not delivered a config"]
}
```

`status` is `ok` or `degraded`; degraded answers with HTTP 503. An instance is
degraded when a configured provisioning endpoint has not delivered, a
configured tunnel is down, or an enabled connection has no registered tools.

The image declares a `HEALTHCHECK` that runs the binary against its own
endpoint (`mux --health-check`), because distroless has no shell and no curl.
It exits 0 on `ok` and 1 on anything else, printing the problems to stderr.
That makes `depends_on: condition: service_healthy` mean what it should:

```yaml
services:
  mux:
    image: mux:dev
    environment:
      MUX_PROVISIONING_ENDPOINT: https://example.com/api/mux/provision
      MUX_PROVISIONING_TOKEN: ${MUX_PROVISIONING_TOKEN}

  app:
    image: your-app
    depends_on:
      mux:
        condition: service_healthy
```

`start-period` is 60s, which covers provisioning and tunnel setup on a cold
start. Raise it if your tunnels are slow to come up.

## Known limitation: the MCP endpoint is loopback-only

The compose example above will not work yet for an app that talks to mux over
the network. mux binds its plain HTTP listener to `127.0.0.1` — deliberately,
because `/mcp` has no authentication of its own and the loopback bind is what
keeps it private. In a container that means the container's own loopback: the
health check reaches it, a sibling service does not.

Verified: a sibling container on the same network gets `connection refused`,
while `mux --health-check` inside the container succeeds.

Until mux can bind elsewhere, a co-located app has to share the network
namespace rather than address mux by service name:

```yaml
services:
  mux:
    image: mux:dev
  app:
    image: your-app
    network_mode: "service:mux"    # mux is reachable at 127.0.0.1:7700
    depends_on:
      mux:
        condition: service_healthy
```

This works, at the cost of one shared network namespace: the two services
cannot both bind the same port, and neither addresses the other by name.
