# Hetzner Cloud

## Prerequisites

- A Hetzner Cloud project. Everything the connection sees belongs to that one project — a second project is a second connection.
- An API token of the project: **Hetzner Console → the project → Security → API Tokens → Generate API Token**. Choose **Read** for a connection that only looks (and set `read_only = true` in the configuration — the form has no switch for it), **Read & Write** for one that changes things. The token is shown once. There are no finer scopes; what a writing token may do through mux is decided by the switches below.
- DNS zones (Hetzner DNS) are part of the project since they moved into the Cloud Console. A project that holds only the zones is the natural home for a DNS connection: its token reaches no servers.

## Fields

| Field | Description |
|------|-------------|
| API Token | The project's token, stored in the Keychain as `{name}-token`. |
| API URL | `https://api.hetzner.cloud` (default). The host only — mux appends `/v1`. |
| Allow lifecycle operations (cost money or destroy data: create, resize, rebuild, power off, delete) | Off by default. On, the connection may run what costs money or destroys data or availability: creating servers, volumes, IPs and load balancers, deleting anything, resizing, changing a server's type, rebuilding, hard power cuts, snapshots and backups, converting a backup into a snapshot, importing a zone file. Not together with `read_only`. |
| Protected resources (optional) | One per line: `name`, `kind:name` or a pattern with `*` (`server:prod-*`, `my-cell-01`, `zone:example.com`). These are never changed, acted on or deleted through this connection, whatever the other switches say; reading them is fine. Snapshots and backups have no name — an `image:` entry matches their description. Kinds: server, image, iso, volume, network, firewall, floating_ip, primary_ip, load_balancer, load_balancer_type, certificate, placement_group, ssh_key, location, server_type, zone. |

`read_only = true` (TOML or provisioning) keeps the reading tools only: list, get, actions, metrics, pricing, record sets, zone file, `api_get`.

## After creating

Click **Test**. mux reads one page of servers and one of zones; the message names how many of each the project has, the mode of the connection (read-only, writes without lifecycle, lifecycle allowed, how many protected entries) and how much of the hourly request budget is left. A wrong or revoked token shows up here with the API's message; a valid token of another project answers with that project's counts — the counts are how you tell the projects apart.

## Notes

- **What a writing connection does without lifecycle.** Labels, names, reverse DNS, firewall rules, routes, adding subnets, attachments (volumes, networks, ISOs, IPs, placement groups), Hetzner's own protection flags, graceful `reboot` and `shutdown`, `poweron`, rescue mode, root password resets, consoles, DNS record sets (create, set, add, remove, delete, TTL) and the free resources (networks, firewalls, SSH keys, placement groups, certificates, zones). Some of these do take a server offline for a moment or hand the agent a root password — the line is money and data, not uptime. The description of the `action` tool marks the lifecycle actions with †.
- **Guards cover the resource a call addresses.** `protected_resources` refuses `update`, `delete`, every `action` and the pass-through on a matching resource (and the record sets of a protected zone; a zone or resource given by name is read once so the guard matches the name Hetzner knows). An operation on another resource that points at a protected one — attaching a volume to it, applying a firewall to it, assigning an IP to it — is not covered, and neither is what Hetzner deletes along with something else: a Primary IP with `auto_delete` goes with its server, backups go with `disable_backup`. Set Hetzner's own delete and rebuild protection on the resources that matter (`action change_protection`); the API refuses the irreversible operations then, mux or not.
- **Actions.** Changes are asynchronous. The tools wait for the API's actions by default (`wait`, `timeout` up to 600 s, polling with growing pauses) and return their final status; a wait that runs out names the ids for `wait_action`. An action that failed makes the result an error with Hetzner's message.
- **Names instead of ids.** Every `id` argument takes the resource's name too; names are unique per project. Zones are addressed by name in the API itself.
- **Request budget.** 3600 requests per hour per project, refilling by one per second. Results say when fewer than 360 are left; a 429 is reported with the time the budget is full again. Polling counts against it — a long `wait` on many actions is paced to about one request every five seconds.
- **Availability of a server type** is in `get server_type` (or `list server_type`): `locations[]` carries per location a `deprecation` object when the type is on its way out there. Prices per location are in the same object; `pricing` has the rest (IPs, volumes, images, backups, load balancer types).
- **DNS.** A record is a record set (`name` + `type`) with one or more records. `set_records` is the one-step way to point a name at an address: it creates the set or replaces its records, and changes the TTL when given. Names are relative to the zone, `@` is the apex; the zone's own SOA and NS sets are among the record sets. `import_zonefile` replaces every record set of a zone and is therefore a lifecycle operation.
- **Pass-through.** `api_get` reads any `/v1/…` path; `api_request` writes and is classified against the same registry as the tools — a delete, a create of a billed resource, a lifecycle action and any write the registry does not know need `allow_lifecycle`, a protected resource is refused.
- **Not part of the type:** Storage Boxes (a different API host, `api.hetzner.com`), the server metadata endpoint (reachable from the server itself only), and the deprecated `/datacenters` endpoints — availability comes from the server type.
