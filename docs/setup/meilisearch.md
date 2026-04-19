# Meilisearch

## Prerequisites

- A running Meilisearch instance.
- The master key or an API key with the required permissions.

## Fields

| Field | Description |
|------|-------------|
| Host | Hostname or IP of the Meilisearch server. |
| Port | Default: `7700`. |
| Index | Name of the default index to access. |
| API Key | Master key or API key. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection. mux calls the health endpoint.

## Notes

- If Meilisearch was started without a master key, the API Key field can be left blank.
- Tunnels are supported for instances that are not directly reachable.
