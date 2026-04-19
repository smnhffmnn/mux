# MariaDB

## Prerequisites

- A running MariaDB or MySQL server.
- A user with the required permissions.

## Fields

| Field | Description |
|------|-------------|
| Host | Hostname or IP of the server. |
| Port | Default: `3306`. |
| User | Database username. |
| Password | Password. Stored in the Keychain. |
| Database | Name of the database. |

## After creating

Click **Test** to verify the connection. mux displays the server version.

## Notes

- Also works with MySQL servers.
- Tunnels are supported for servers that are not directly reachable.
