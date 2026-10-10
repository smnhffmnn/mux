#!/usr/bin/env bash
# Refresh the vendored YouTrack OpenAPI document that the youtrack-rest spec
# tests run against (internal/tools/testdata/youtrack-openapi.json).
#
# YouTrack publishes the document on every instance without authentication;
# JetBrains' own instance is the reference. The snapshot is normalized —
# descriptive text removed, keys sorted — so a refresh diffs on structure only.
# Property *names* called "description" (Issue.description, Project.description)
# are objects and survive; only string-valued "description" keys are prose.
#
# Usage: scripts/youtrack-openapi-snapshot.sh [source-url-or-file]
set -euo pipefail

src="${1:-https://youtrack.jetbrains.com/api/openapi.json}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/tools/testdata/youtrack-openapi.json"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

if [[ "$src" == http* ]]; then
  curl -fsSL --max-time 60 "$src" -o "$tmp"
else
  cp "$src" "$tmp"
fi

jq -S --indent 1 'walk(if type == "object" and (.description? | type) == "string" then del(.description) else . end)' "$tmp" > "$out"
echo "snapshot: $(jq -r '.info.version' "$out"), $(jq '.paths | length' "$out") paths, $(jq '.components.schemas | length' "$out") schemas → $out"
