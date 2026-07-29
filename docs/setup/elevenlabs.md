# ElevenLabs

## Prerequisites

- An ElevenLabs account with API access.
- An API key.

### Creating an API key

1. Open [elevenlabs.io](https://elevenlabs.io/).
2. Go to **Profile → API Keys**.
3. Create a new key and copy it.

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the ElevenLabs API. Default: `https://api.elevenlabs.io`. |
| API Key | Your API key (format: `xi_...`). Stored in the Keychain. |

## After creating

Click **Test** to verify the connection.

## Notes

- Available features (text-to-speech, voice cloning, etc.) depend on your plan.
- The API URL typically does not need to be changed.
- Two tools are exposed: `get` for JSON endpoints (voices, models, subscription) and audio downloads, `post` for text-to-speech and sound generation.
- Generated audio is written to a file instead of being returned inline — pass `output_file` (absolute path) to choose where, otherwise it goes to `~/.mux/output/`.
