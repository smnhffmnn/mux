#!/usr/bin/env bash
# Refresh the vendored Hetzner Cloud API OpenAPI document that the
# hetzner-cloud spec tests run against (internal/tools/testdata/hetzner-openapi.json).
#
# Hetzner publishes the document without authentication at
# https://docs.hetzner.cloud/cloud.spec.json (OpenAPI 3.1; the reference at
# https://docs.hetzner.cloud/reference/cloud renders it). The snapshot is
# normalized — descriptive text, examples and code samples removed, keys
# sorted — so a refresh diffs on structure only.
#
# info.version is "1.0.0" in every revision; the document carries no other
# version. The marker that moves is the sha256 of the normalized document,
# recorded with the fetch date next to the snapshot.
#
# Usage: scripts/hetzner-openapi-snapshot.sh [source-url-or-file]
set -euo pipefail

src="${1:-https://docs.hetzner.cloud/cloud.spec.json}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/tools/testdata/hetzner-openapi.json"
version="$root/internal/tools/testdata/hetzner-openapi.version"
tmp="$(mktemp)"
trap 'rm -f "$tmp" "$tmp.out"' EXIT

if [[ "$src" == http* ]]; then
  curl -fsSL --max-time 120 "$src" -o "$tmp"
else
  cp "$src" "$tmp"
fi

# A structural snapshot for the Go tests, not a valid OpenAPI document
# (required prose such as responses.*.description is gone). Written via a
# temp file so a failed fetch or filter never leaves a truncated snapshot.
jq -S --indent 1 -f "$root/scripts/hetzner-openapi-normalize.jq" "$tmp" > "$tmp.out"
mv "$tmp.out" "$out"

ops="$(jq '[.paths[] | to_entries[] | select(.key | IN("get","post","put","patch","delete"))] | length' "$out")"
deprecated="$(jq '[.paths[] | to_entries[] | select(.key | IN("get","post","put","patch","delete")) | select(.value.deprecated == true)] | length' "$out")"
{
  echo "source: $src"
  echo "fetched: $(date -u +%Y-%m-%d)"
  echo "sha256: $(sha256sum "$out" | cut -c1-16)"
  echo "openapi: $(jq -r '.openapi' "$out"), info.version $(jq -r '.info.version' "$out")"
  echo "paths: $(jq '.paths | length' "$out"), operations: $ops, deprecated: $deprecated"
} > "$version"
echo "snapshot: $(jq -r '.info.title + " " + .info.version' "$out"), $(jq '.paths | length' "$out") paths, $ops operations ($deprecated deprecated) → $out"
echo "version: $(sed -n 3p "$version") → $version"
