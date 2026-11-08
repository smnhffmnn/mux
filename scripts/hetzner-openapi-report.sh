#!/usr/bin/env bash
# Report what changed between a live Hetzner Cloud OpenAPI document and the
# vendored snapshot: added/removed paths, changed methods, operations newly
# marked deprecated. Structure only — the spec tests already fail on changes
# to the endpoints, parameters and fields mux uses; this lists the rest so
# new API surface is visible as "not built yet" and a new deprecation is
# read before Hetzner removes the endpoint (410 deprecated_api_endpoint).
#
# Usage: scripts/hetzner-openapi-report.sh <live.json> <snapshot.json>
set -euo pipefail

live="$1"
snap="$2"
root="$(cd "$(dirname "$0")/.." && pwd)"
norm="$root/scripts/hetzner-openapi-normalize.jq"

if [[ -f "$root/internal/tools/testdata/hetzner-openapi.version" ]]; then
  sed 's/^/snapshot /' "$root/internal/tools/testdata/hetzner-openapi.version"
fi
echo "live: openapi $(jq -r '.openapi' "$live"), info.version $(jq -r '.info.version' "$live"), sha256 $(jq -S --indent 1 -f "$norm" "$live" | sha256sum | cut -c1-16), $(jq '.paths | length' "$live") paths"
echo

paths_live="$(jq -r '.paths | keys[]' "$live" | sort)"
paths_snap="$(jq -r '.paths | keys[]' "$snap" | sort)"
added="$(comm -13 <(echo "$paths_snap") <(echo "$paths_live"))"
removed="$(comm -23 <(echo "$paths_snap") <(echo "$paths_live"))"
count() { if [[ -n "$1" ]]; then printf '%s\n' "$1" | wc -l | tr -d ' '; else echo 0; fi; }
echo "paths added ($(count "$added")):"; echo "${added:-none}" | sed 's/^/  /'
echo "paths removed ($(count "$removed")):"; echo "${removed:-none}" | sed 's/^/  /'
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

dep_filter='[.paths | to_entries[] | .key as $p | .value | to_entries[] | select(.key | IN("get","post","put","patch","delete")) | select(.value.deprecated == true) | "\(.key | ascii_upcase) \($p)"] | .[]'
dep_live="$(jq -r "$dep_filter" "$live" | sort)"
dep_snap="$(jq -r "$dep_filter" "$snap" | sort)"
echo "operations newly deprecated:"; comm -13 <(echo "$dep_snap") <(echo "$dep_live") | sed 's/^/  /' | grep . || echo "  none"
echo "deprecated operations in the live document ($(count "$dep_live")):"; echo "${dep_live:-none}" | sed 's/^/  /'
echo

if diff -q <(jq -S --indent 1 -f "$norm" "$live") "$snap" >/dev/null; then
  echo "structure identical to the snapshot"
else
  echo "structure differs from the snapshot — refresh with scripts/hetzner-openapi-snapshot.sh and review the diff"
fi
