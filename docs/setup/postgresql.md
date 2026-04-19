# PostgreSQL

## Prerequisites

- A running PostgreSQL server reachable from the mux host.
- A database user with the required permissions (at least `SELECT` for read-only).
- If the server is not directly reachable: create a tunnel in mux and select it when creating the connection.

## Fields

| Field | Description |
|------|-------------|
| Host | Hostname or IP of the PostgreSQL server. |
| Port | Default: `5432`. |
| User | Database username. |
| Password | User's password. Stored in the Keychain. |
| Database | Name of the database to access. |

## After creating

Click **Test** to verify the connection. mux displays the PostgreSQL version on success.

## Notes

- When connecting through a tunnel: create and connect the tunnel first, then create the connection.
- `sslmode=disable` is set automatically — for TLS connections, the server must be configured accordingly.
