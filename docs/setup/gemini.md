# Google Gemini

## Prerequisites

- A Google account with access to the Gemini API.
- An API key.

### Creating an API key

1. Open [Google AI Studio](https://aistudio.google.com/).
2. Click **Get API Key**.
3. Create a key for a Google Cloud project and copy it.

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the Gemini API. Default: `https://generativelanguage.googleapis.com/v1beta`. |
| API Key | Your API key (format: `AIzaSy...`). Stored in the Keychain. |

## After creating

Click **Test** to verify the connection. mux fetches the list of models.

## Notes

- The key is sent as an `x-goog-api-key` header.
- The free tier has rate limits — use a billing-enabled project for production workloads.
