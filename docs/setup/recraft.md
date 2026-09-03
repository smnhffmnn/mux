# Recraft

## Prerequisites

- A Recraft account with API access.
- An API token.

### Getting a token

1. Open [recraft.ai](https://www.recraft.ai/).
2. Go to the API settings in your account.
3. Create a token and copy it.

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the Recraft API. Default: `https://external.api.recraft.ai/v1`. |
| API Key | Your API token. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection.

## Notes

- Recraft offers image generation and editing.
- The API URL typically does not need to be changed.
- Image editing endpoints (`/images/vectorize`, `/images/removeBackground`, `/images/imageToImage`, …) take a local image via `file_path` on the `post` tool.
