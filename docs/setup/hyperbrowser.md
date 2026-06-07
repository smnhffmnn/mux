# Hyperbrowser

Stealth headless Chrome with residential proxy rotation and CAPTCHA solving via the Hyperbrowser API. Use it when Firecrawl fails on anti-bot-protected sites (Imperva, Cloudflare, DataDome) or JS-heavy SPAs that need a real browser.

## Prerequisites

- A Hyperbrowser account and API key.
- Proxy usage and CAPTCHA solving require a paid plan.

### Getting an API key

1. Sign up at [hyperbrowser.ai](https://hyperbrowser.ai/).
2. Open the dashboard and create an API key.
3. Copy it (format: `hb_...`).

## Fields

| Field | Description |
|------|-------------|
| API URL | URL of the Hyperbrowser API. Default: `https://api.hyperbrowser.ai`. |
| API Key (x-api-key) | Your Hyperbrowser API key (format: `hb_...`). Sent as the `x-api-key` header. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection.

## Notes

- Stateless scraping: `scrape`, `extract`, `crawl`, `crawl_status`. Start here — cheapest and usually enough.
- Sessions: `session_create`, `session_stop`, `session_list`. Persistent browsers are billed while active — always stop them when done.
- Browser-Use agent: `browser_use_agent_start`, `browser_use_agent_status`. LLM-driven navigation for sites where direct scraping is blocked. Costs roughly $0.10/run plus LLM tokens.
- `session_options` is a comma-separated list: `use_stealth`, `use_proxy`, `solve_captchas`, `accept_cookies`. Setting `proxy_country` implicitly enables `use_proxy`.
