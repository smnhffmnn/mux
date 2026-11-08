#!/usr/bin/env bash
# Refresh the vendored bunq OpenAPI document that the bunq spec tests run
# against (internal/tools/testdata/bunq-openapi.json).
#
# bunq publishes its contract as swagger.json in https://github.com/bunq/doc
# (OpenAPI 3.0; production and sandbox share it). The snapshot is normalized
# — descriptive text and examples removed, keys sorted — so a refresh diffs
# on structure only.
#
# Usage: scripts/bunq-openapi-snapshot.sh [source-url-or-file]
set -euo pipefail

# bunq_latest_tag prints the newest tag of github.com/bunq/doc — the only
# moving version marker bunq publishes (git is addressed with its remote
# URL, no working tree involved).
bunq_latest_tag() {
  GIT_TERMINAL_PROMPT=0 git ls-remote --tags --refs https://github.com/bunq/doc.git 2>/dev/null \
    | sed -E 's#.*refs/tags/##' | sort -V | tail -1
}

src="${1:-https://raw.githubusercontent.com/bunq/doc/develop/swagger.json}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/tools/testdata/bunq-openapi.json"
version="$root/internal/tools/testdata/bunq-openapi.version"
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
jq -S --indent 1 -f "$root/scripts/openapi-normalize.jq" "$tmp" > "$tmp.out"
mv "$tmp.out" "$out"

# info.version is "1.0" forever; the version bunq actually moves is the tag
# of the bunq/doc repository ("Regenerated code based on the latest tag").
# Recorded next to the snapshot so the report can say which release it is.
tag="$(bunq_latest_tag || true)"
{
  echo "source: $src"
  echo "fetched: $(date -u +%Y-%m-%d)"
  echo "sha256: $(sha256sum "$out" | cut -c1-16)"
  echo "bunq/doc tag: ${tag:-unknown}"
} > "$version"
echo "snapshot: $(jq -r '.info.version' "$out"), $(jq '.paths | length' "$out") paths, $(jq '.components.schemas | length' "$out") schemas → $out"
echo "version: bunq/doc tag ${tag:-unknown} → $version"
