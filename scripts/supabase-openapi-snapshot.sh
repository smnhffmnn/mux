#!/usr/bin/env bash
# Refresh the vendored Supabase Management API OpenAPI document that the
# supabase spec tests run against (internal/tools/testdata/supabase-openapi.json).
#
# Supabase publishes the document without authentication at
# https://api.supabase.com/api/v1-json (OpenAPI 3.0; the Scalar reference page
# at /api/v1 renders it). The snapshot is normalized — descriptive text and
# examples removed, keys sorted — so a refresh diffs on structure only.
# Property *names* called "description" are objects and survive; only
# string-valued "description" keys are prose.
#
# Usage: scripts/supabase-openapi-snapshot.sh [source-url-or-file]
set -euo pipefail

src="${1:-https://api.supabase.com/api/v1-json}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/tools/testdata/supabase-openapi.json"
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
jq -S --indent 1 -f "$root/scripts/supabase-openapi-normalize.jq" "$tmp" > "$tmp.out"
mv "$tmp.out" "$out"
echo "snapshot: $(jq -r '.info.title + " " + .info.version' "$out"), $(jq '.paths | length' "$out") paths, $(jq '.components.schemas | length' "$out") schemas → $out"
