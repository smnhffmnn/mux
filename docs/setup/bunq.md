# bunq

## Prerequisites

- A bunq account (personal or business) and the bunq app.
- An API key, created in the app: **Settings → Developers → API keys → add an API key**. The key is shown once; bunq binds it to the IP address it is first used from (see Notes).
- For a try-out without a real account: the sandbox at `https://public-api.sandbox.bunq.com`. A sandbox user and key come from `POST /v1/sandbox-user-person` on that host — no sign-up; test money from bunq's "sugardaddy" (see [bunq's sandbox page](https://doc.bunq.com/sandbox-users-and-sandbox-money)).

## Fields

| Field | Description |
|------|-------------|
| API URL | `https://api.bunq.com` (default) or `https://public-api.sandbox.bunq.com`. The host only — mux appends `/v1`. |
| API Key | The key from the app, stored in the Keychain as `{name}-token`. |

`read_only = true` (TOML or provisioning) keeps the reading tools only: no `create_draft_payment`, no `api_request`.

## What mux creates on first use

bunq's API has a way in before the first read: an **installation** (an RSA public key, answered with an installation token), a **device** (the API key, registered under that installation) and a **session** (a token that lives as long as the auto-logout time set in the app). mux does all three itself the first time a tool runs or **Test** is clicked:

- It generates a 2048-bit RSA key and stores it as the secret `{name}-private-key`. Every request with a body is signed with it (`X-Bunq-Client-Signature`).
- It stores installation token, server key, device id and the session under the secret `{name}-api-context`, each step as soon as it succeeds, so a restart neither registers a new installation (bunq allows only a few per day) nor a new device, and a refused device registration (wrong key, key bound to another address) does not cost a second installation. The context is tied to the API host, the RSA key and the API key: a context from the sandbox is not used against production, a new RSA key leads to a new installation, a rotated API key keeps the installation and registers a new device.
- When a session has run out, the next call signs in again. When bunq refuses the installation token itself (401 or 403 on the device or session registration — a revoked key), mux registers once more.

Deleting the connection deletes the key and the context with it.

## After creating

Click **Test**. mux signs in and lists the accounts; the message names the user the key belongs to, the number of accounts (active ones counted separately), until when the session lasts, and whether the connection points at the sandbox. A key that bunq refuses from this IP, a revoked key or a wrong host shows up here.

## Notes

- **The IP binding.** bunq ties a standard API key to the IP address of its first device registration and does not let the API add addresses later; a key used from another address gets a refusal that names the IP. Run mux where the key was first used (a server with a fixed address is the simple case), or switch the key to "Allow All IP Addresses" in the app. The connection reports the refusal as such instead of registering again.
- **Money moves only as a draft — and the guard is mux's.** `create_draft_payment` files a transfer the account's owner accepts in the bunq app; until then nothing leaves the account, and `list_draft_payments` shows the draft as `PENDING`. bunq does not enforce the app: an API key may accept its own draft with `PUT …/draft-payment/{id}` and `status: ACCEPTED`, and the money moves at once (verified in the sandbox on 2026-11-08). mux therefore never sends that call: the direct payment endpoints are not part of the type, `api_request` refuses them along with requests, cards, scheduled payments and every write to a draft other than creating one with a required acceptance, and a test pins it. There is no switch that changes this. What remains is the key itself — anyone holding the API key outside mux has the same power the app has.
- **Rate limits.** bunq throttles per endpoint (three reads per three seconds); a 429 is passed on with that note. `list_payments` with `since` pages at most 20 times, about a second apart, and returns what it has when a page fails.
- **Sessions.** A session lasts the auto-logout time set in the app (a week is common) and is renewed before it ends or after bunq refuses it. The RSA key never leaves the secret store; the API key goes to bunq in the device and session registration and nowhere else; the session token is kept in the stored context.
- **Statements.** `export_statement` asks bunq for a CSV, MT940 or PDF statement of a date range and waits up to 30 s for bunq to produce it; a statement that is not ready by then is reported with its id and the path to fetch it later.
- Timestamps come back as bunq sends them (`YYYY-MM-DD HH:MM:SS.ffffff`, UTC); amounts are strings with two decimals plus a currency.
