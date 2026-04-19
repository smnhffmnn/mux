# fal.ai

## Prerequisites

- A fal.ai account with API access.
- An API key.

### Creating an API key

1. Open [fal.ai](https://fal.ai/).
2. Go to **Dashboard → Keys**.
3. Create a new key and copy it.

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the fal.ai queue API. Default: `https://queue.fal.run`. |
| API Key | Your API key (format: `fal_...`). Stored in the Keychain. |

## After creating

Click **Test** to verify the connection.

## Notes

- fal.ai exposes an async inference queue for model execution (image generation, video generation, and other generative workloads).
- Exposed tools: `submit` (enqueue a job), `status` (poll job status), `result` (fetch the finished output).
- The key is sent as `Authorization: Key <token>` (note: `Key`, not `Bearer`).
- The API URL typically does not need to be changed.
