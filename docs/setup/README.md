# Setup Docs

One Markdown file per connection type, rendered inline in the desktop app:

- in the **Add Connection** dialog (preview for the selected type)
- in each **Connection Card** under a collapsible *Setup Guide* toggle

Files are embedded into the binary via `embed.go` (`go:embed *.md`). The filename must match the connection type key from [`internal/config/types.go`](../../internal/config/types.go) — e.g. `fal-ai.md` for `type = "fal-ai"`. Missing docs are rendered as an empty string; the toggle is hidden.

## Template

```markdown
# <Human-readable name>

## Prerequisites

- External requirements (account, plugin, permissions).

### Creating a token

<Step-by-step, only if applicable.>

## Fields

| Field | Description |
|------|-------------|
| <Field> | <What to enter. Mention defaults and where secrets are stored.> |

## After creating

<What to click next — typically `Test`, occasionally `Authorize` for OAuth flows.>

## Notes

<Quirks, limits, gotchas.>
```

## Conventions

- English only. Match the tone of [`../connections.md`](../connections.md) and [`../google-workspace-setup.md`](../google-workspace-setup.md) — technical, concise, no fluff.
- Use the section headings `Prerequisites`, `Fields`, `After creating`, `Notes` consistently. Additional sections are fine when a connection needs them.
- Field names in the table must match the `Label` strings from `types.go`.
- Mention where secrets land (`Keychain`, vault, etc.).
- Keep it tight — aim for under 40 lines. The dialog preview is narrow.

The rendered Markdown is sanitized with DOMPurify before being inserted into the DOM, so raw HTML inside docs will be stripped. Stick to plain Markdown.
