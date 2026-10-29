#!/usr/bin/env bash
# Report what changed between a live Nylas OpenAPI document and the vendored
# snapshot: version, added/removed paths, changed methods, added/removed
# schemas. Structure only — the spec tests already fail on changes to the
# endpoints and fields mux uses; this lists the rest so new API surface is
# visible as "not built yet".
#
# Usage: scripts/nylas-openapi-report.sh <live.json> <snapshot.json>
set -euo pipefail

live="$1"
snap="$2"
root="$(cd "$(dirname "$0")/.." && pwd)"
norm="$root/scripts/nylas-openapi-normalize.jq"

echo "snapshot: $(jq -r '.info.version' "$snap")   live: $(jq -r '.info.version' "$live")"
echo

paths_live="$(jq -r '.paths | keys[]' "$live" | sort)"
paths_snap="$(jq -r '.paths | keys[]' "$snap" | sort)"
added="$(comm -13 <(echo "$paths_snap") <(echo "$paths_live"))"
removed="$(comm -23 <(echo "$paths_snap") <(echo "$paths_live"))"
count() { if [[ -n "$1" ]]; then printf '%s\n' "$1" | wc -l | tr -d ' '; else echo 0; fi; }
echo "paths added ($(count "$added")):"; echo "${added:-  none}" | sed 's/^/  /'
echo "paths removed ($(count "$removed")):"; echo "${removed:-  none}" | sed 's/^/  /'
echo

echo "methods changed on common paths:"
changed=0
while IFS= read -r p; do
  ml="$(jq -r --arg p "$p" '.paths[$p] | keys | map(select(. == "get" or . == "post" or . == "put" or . == "patch" or . == "delete")) | sort | join(",")' "$live")"
  ms="$(jq -r --arg p "$p" '.paths[$p] | keys | map(select(. == "get" or . == "post" or . == "put" or . == "patch" or . == "delete")) | sort | join(",")' "$snap")"
  if [[ "$ml" != "$ms" ]]; then
    echo "  $p: $ms → $ml"
    changed=1
  fi
done < <(comm -12 <(echo "$paths_snap") <(echo "$paths_live"))
[[ $changed -eq 0 ]] && echo "  none"
echo

schemas_live="$(jq -r '.components.schemas | keys[]' "$live" | sort)"
schemas_snap="$(jq -r '.components.schemas | keys[]' "$snap" | sort)"
echo "schemas added:"; comm -13 <(echo "$schemas_snap") <(echo "$schemas_live") | sed 's/^/  /' | grep . || echo "  none"
echo "schemas removed:"; comm -23 <(echo "$schemas_snap") <(echo "$schemas_live") | sed 's/^/  /' | grep . || echo "  none"
echo

if diff -q <(jq -S --indent 1 -f "$norm" "$live") "$snap" >/dev/null; then
  echo "structure identical to the snapshot"
else
  echo "structure differs from the snapshot — refresh with scripts/nylas-openapi-snapshot.sh and review the diff"
fi
