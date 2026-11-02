# Blobs — files between the agent and mux

mux does not read files from the agent's machine and does not write files to it. When a tool needs a file, the agent hands it to mux first; when a tool produces one, mux keeps it and hands back a reference the agent fetches. This is the blob store: a temporary file store on mux's own loopback listener, next to `/mcp` and `/health`.

The reason is the topology. On a laptop, agent and mux share a disk, and a path in a tool call is enough. In a container — an agent host, a CI job, a sibling service — mux has its own file system, and a path from the agent names nothing there (`path not found: … (resolved on the machine running mux)`). Mounting the agent's directories into the mux container would make mux a service that reads other people's disks, which is not what a gateway is. The blob store is what Composio does with S3 staging and what n8n Cloud does by taking files from other services only: bytes reach the service through an interface, never through a path.

Where agent and mux do share a disk, absolute paths keep working as before. The blob store is the way that works everywhere.

## Routes

All on the loopback listener (`http://127.0.0.1:<port>`, port 7700 by default). No token of their own: reaching the listener is the credential, as for `/mcp`. Two checks keep a browser on the same machine out, because a page can reach loopback too: the `Host` header has to be a loopback name (`127.0.0.1`, `localhost`, `::1`), and a request a browser marks as cross-site (an `Origin` that is not loopback, `Sec-Fetch-Site: cross-site`) is refused with `403`. `curl` sends neither header. Every download is served as an attachment (`Content-Disposition`), with sniffing off and scripts sandboxed, so a blob holding HTML or SVG from some remote API never runs as a page on this origin.

| Route | What it does |
|---|---|
| `POST /blob` | Store the request body as a blob. `Content-Type` is kept; `?name=<file name>` or `X-Blob-Name` names it (an attachment needs a name). A `multipart/form-data` body is accepted too: the first file part is the blob. `?tree=1` (or `X-Blob-Kind: tree`) unpacks a **tar, tar.gz or zip** archive into a directory tree instead. Answers `201` with the blob's description, its `url` and its `ref` (`blob:<id>`). |
| `GET /blob` | The live blobs, newest first, with the store's limits. |
| `GET /blob/{id}` | The bytes, as an attachment with the stored `Content-Type` and the name (or the id plus an extension). A tree comes back as **tar.gz**; `?list=1` lists its files instead. |
| `DELETE /blob/{id}` | Delete it. Deleting an unknown id is fine. |

Errors are JSON (`{"error": …}`): `400` for a malformed id, or an archive whose content is refused (an entry that escapes, a duplicate name, no file at all); `403` for a request that is not from this machine; `404` for an unknown or expired blob; `413` over the per-blob cap or the entry cap; `415` for a tree upload whose bytes are not a tar, tar.gz or zip; `500` when the disk fails.

```bash
# a file
curl -sS --data-binary @photo.png -H 'Content-Type: image/png' -H 'X-Blob-Name: photo.png' http://127.0.0.1:7700/blob
# → {"id":"4f2a…","kind":"file","name":"photo.png","contentType":"image/png","size":48213,"expires":"…","url":"http://127.0.0.1:7700/blob/4f2a…","ref":"blob:4f2a…"}

# a directory, for the Supabase deploy and migrations
tar -C ~/projects/myproject -cz supabase | curl -sS --data-binary @- -H 'Content-Type: application/gzip' 'http://127.0.0.1:7700/blob?tree=1&name=myproject'
# → {"id":"9c01…","kind":"tree","files":221,"size":1834022,"topLevel":["supabase"],…}

# fetch a result
curl -sS -o report.pdf http://127.0.0.1:7700/blob/7b3e…
curl -sS http://127.0.0.1:7700/blob/9c01… | tar -xz -C ./restored
```

## In tool calls

Two spellings, both plain strings in the arguments a tool already has:

- **Give a blob:** `blob:<id>` wherever a tool takes a file or directory — `file_path` (http, OpenAI, Recraft, Ideogram requests; SSH upload), `file_paths` (YouTrack attach), `images` (Gemini), `env_file` (Supabase secrets) and `path` (Supabase deploy and migrations, which take a tree). An absolute path is still accepted where agent and mux share a disk.
- **Receive a blob:** `output_file = "blob"` (or `output_dir = "blob"`) wherever a tool writes a file — the result then names a blob instead of a path. Without `output_file`, a tool stores as a blob whatever it would otherwise have written to mux's own disk: binary responses (ElevenLabs audio, Gemini images, YouTrack attachments) and Microsoft Graph attachments. A 2xx response longer than the connector's inline limit is no longer cut off in silence: the first bytes come back as the preview they always were, and a marker names the blob holding the whole body (error responses are cut with a note and not stored). Images up to 5 MB also come back as image content, so they can be looked at without a round trip.

The result always says what happened, never the bytes:

```
Stored as blob 7b3e… (application/pdf, 1.2 MB, name "Q3 report.pdf", expires 2026-11-01 14:23 UTC) — fetch: curl -sS -o FILE http://127.0.0.1:7700/blob/7b3e…; pass on as blob:7b3e…
```

The name is shown quoted and never written into the command: names come from mail senders, issue trackers and remote hosts, and a name in a shell line would be a shell line. The agent picks `FILE`.

## Lifetime

- A blob lives **one hour** (`[blobs] ttl`), then it is gone, whether or not anything still refers to it.
- A blob a **write used is deleted afterwards**: a successful Supabase deploy, `migrations_apply` that ran every pending file, an upload that got a 2xx. A read leaves it — `function_deploy dry_run` and `migrations_pending` use the same upload as the deploy that follows, and a failed call leaves the blob for the retry. The one exception is `secrets_set`: an uploaded `.env` is deleted the moment it has been read, whatever happens next, because a file of secrets has no business staying in the store.
- Each process has its **own directory** (`blobs/<pid>`), emptied when it starts and removed when it stops normally; a second mux started beside a running one (a terminal next to the tray app) does not touch the running one's blobs, and directories of processes that are gone are reaped at the next start. Ids die with the process.
- **Caps:** 1 GiB for all blobs together (`max_total`), 256 MiB per blob (`max_blob`); the oldest blobs go first when the cap is reached. A tree is capped at 50,000 entries — files, directories and skipped entries together. A command's stdout or an SFTP download that goes to a blob stops at the per-blob cap; a path has no limit.
- The store lives on disk under the state directory (`$XDG_STATE_HOME/mux/blobs`, `/state/mux/blobs` in the container), not in memory — the container's memory limit is not the disk's.

## Trees

A tree is an archive unpacked once, so a tool that walks files (the Supabase deploy follows the import graph; the migrations read a directory) finds them under a plain path. Unpacking is strict: an entry whose name would land outside the tree (absolute, or with a `..` segment anywhere) fails the whole upload rather than being remapped; symbolic and hard links, devices and FIFOs are skipped and counted (`skipped`); the written bytes are capped like a single blob and every entry counts against the entry cap.

macOS `tar` and Finder zips add metadata sidecars (`._name` AppleDouble files, a `__MACOSX` folder); mux skips them and counts them under `skipped`, so `COPYFILE_DISABLE=1` before `tar` is tidier but not required.

The archive's top level is the project root the tool sees. For Supabase that means `supabase/` at the top: `tar -C <project root> -cz supabase`. The upload's answer lists `topLevel`, and a deploy that finds no `supabase/functions` says what it found instead.

## What stays on the agent's side

mux has no helper for the agent's shell; `curl` and `tar` are enough, and the MCP server instructions carry the commands. Where mux and the agent share a disk, nothing needs to change.

## Configuration

```toml
[blobs]
# ttl = "1h"          # lifetime of a blob
# max_total = "1GiB"  # all blobs together, on disk
# max_blob = "256MiB" # one blob, or one unpacked tree
```

Sizes take K/M/G/T with or without an "i"; both are binary. The store is always on when mux serves HTTP; a plain stdio instance without a listener has none, and its file tools say so when asked for a blob.

## Security

The trust boundary is the loopback listener, as for `/mcp`: any process on this machine that reaches the port can list, fetch and delete every blob. The `Host` and cross-site checks above keep a browser page out; a token on the listener is the step that would change the boundary itself. Keep the store in mind on a shared host. An uploaded `.env` is deleted the moment it has been read; everything else lives for the TTL, and the process removes its directory when it stops normally (after a crash, the next start reaps it).
