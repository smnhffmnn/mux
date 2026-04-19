# OpenAI

## Prerequisites

- An OpenAI account with API access.
- An API key.

### Creating an API key

1. Open [platform.openai.com](https://platform.openai.com/).
2. Go to **API Keys**.
3. Create a new key and copy it.

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the OpenAI API. Default: `https://api.openai.com`. Change for compatible APIs (e.g. Azure OpenAI, local LLMs). |
| API Key | Your API key (format: `sk-...`). Stored in the Keychain. |

## After creating

Click **Test** to verify the connection. mux fetches the list of models.

## Notes

- Also works with OpenAI-compatible APIs (e.g. Ollama, LiteLLM) — adjust the API URL accordingly.
- The API key requires credit on the OpenAI account.
