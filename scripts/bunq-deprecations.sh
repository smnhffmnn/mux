#!/usr/bin/env bash
# Watch bunq's prose documentation for deprecations that touch what mux uses.
#
# bunq versions its API by prose, not by number: the path stays /v1, the
# OpenAPI info.version stays "1.0", and swagger.json carries no deprecated
# flags — deprecations appear as "[deprecated since YYYYMMDD]" in the titles
# of the API reference (https://doc.bunq.com/sitemap.md). This script lists
# those titles, keeps the ones that name a resource the bunq type calls, and
# compares them with the vendored list; a new one fails the run so it is
# looked at.
#
# Usage: scripts/bunq-deprecations.sh [--update] [sitemap-url-or-file]
#   --update  rewrite the vendored list from the live document
set -euo pipefail

update=0
if [[ "${1:-}" == "--update" ]]; then
  update=1
  shift
fi
src="${1:-https://doc.bunq.com/sitemap.md}"
root="$(cd "$(dirname "$0")/.." && pwd)"
known="$root/internal/tools/testdata/bunq-deprecations.txt"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

if [[ "$src" == http* ]]; then
  curl -fsSL --max-time 60 "$src" -o "$tmp"
else
  cp "$src" "$tmp"
fi

# The resources the type calls (bunqEndpoints in internal/tools/bunq.go) and
# the pass-through writes it allows.
resources='installation|device-server|session-server|monetary-account|payment|event|draft-payment|customer-statement|statement|note-text|note-attachment|export-annual-overview|export-rib'

# One line per entry: the title with its link, the trailing summary cut.
live="$(grep -i 'deprecat' "$tmp" | sed -E 's/^[[:space:]*-]+//; s/\): .*$/)/; s/[[:space:]]+$//' | grep -Ei "$resources" | sort -u || true)"
echo "deprecations in bunq's reference that name a resource mux uses ($(printf '%s\n' "$live" | grep -c . || true)):"
echo "${live:-  none}" | sed 's/^/  /'
echo

if [[ $update -eq 1 ]]; then
  printf '%s\n' "$live" > "$known"
  echo "vendored list rewritten: $known"
  exit 0
fi

touch "$known"
new="$(comm -13 <(sort -u "$known") <(printf '%s\n' "$live" | grep . || true))"
if [[ -n "$new" ]]; then
  echo "NEW since the vendored list — read them, then run scripts/bunq-deprecations.sh --update:"
  echo "$new" | sed 's/^/  /'
  exit 1
fi
echo "nothing new since the vendored list"
