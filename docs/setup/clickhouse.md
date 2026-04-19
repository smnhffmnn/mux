# ClickHouse

## Prerequisites

- A running ClickHouse server with the HTTP interface enabled.
- A user with read access to the target database.

## Fields

| Field | Description |
|------|-------------|
| Host | Hostname or IP of the ClickHouse server. |
| Port | HTTP port. Default: `8123`. |
| User | Username. Default: `default`. |
| Password | Password. Stored in the Keychain. |
| Default Database | Database queried by default. |

## After creating

Click **Test** to verify the connection. mux displays the ClickHouse version.

## Notes

- mux connects via the HTTP interface (not the native TCP protocol).
- Tunnels are supported if the server is not directly reachable.
