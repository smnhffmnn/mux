#!/usr/bin/env bash
# Refresh the vendored Nylas OpenAPI document that the nylas spec tests run
# against (internal/tools/testdata/nylas-openapi.json).
#
# Nylas publishes the v3 contract without authentication at
# https://developer.nylas.com/openapi.json (OpenAPI 3.1, both regions share
# it). The snapshot is normalized — descriptive text and examples removed, keys
# sorted — so a refresh diffs on structure only. Property *names* called
# "description" are objects and survive; only string-valued "description" keys
# are prose.
#
# Usage: scripts/nylas-openapi-snapshot.sh [source-url-or-file]
set -euo pipefail

src="${1:-https://developer.nylas.com/openapi.json}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/tools/testdata/nylas-openapi.json"
tmp="$(mktemp)"
trap 'rm -f "$tmp" "$tmp.out"' EXIT

if [[ "$src" == http* ]]; then
  curl -fsSL --max-time 120 "$src" -o "$tmp"
else
  cp "$src" "$tmp"
fi

# The result is a structural snapshot for the Go tests, not a valid OpenAPI
# document (required prose such as responses.*.description is gone). Written
# via a temp file so a failed fetch or filter never leaves a truncated snapshot.
# shellcheck source=scripts/nylas-openapi-normalize.jq
jq -S --indent 1 -f "$root/scripts/nylas-openapi-normalize.jq" "$tmp" > "$tmp.out"
mv "$tmp.out" "$out"
echo "snapshot: $(jq -r '.info.version' "$out"), $(jq '.paths | length' "$out") paths, $(jq '.components.schemas | length' "$out") schemas → $out"
