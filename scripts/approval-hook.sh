#!/usr/bin/env bash
# Mux Vault Approval Hook for Claude Code
#
# PreToolUse hook that intercepts privileged actions, creates an approval
# request in Mux, notifies the user (Discord), and blocks until approved.
#
# Usage in settings.json:
#   {
#     "hooks": {
#       "PreToolUse": [{
#         "matcher": "Bash",
#         "command": "/path/to/mux/scripts/approval-hook.sh"
#       }]
#     }
#   }
#
# Requires: jq, curl
#
# Environment variables:
#   MUX_URL       — Mux base URL (default: http://localhost:7700)
#   MUX_CONTEXT   — Context label for approvals (default: from git remote)
#
# Exit codes:
#   0 — approved (or not a privileged action)
#   2 — denied or expired

set -euo pipefail

MUX_URL="${MUX_URL:-http://localhost:7700}"
POLL_INTERVAL=2  # seconds
MAX_WAIT=600     # 10 minutes

# Verify jq is available
if ! command -v jq &>/dev/null; then
    echo "Error: jq is required but not installed" >&2
    exit 2
fi

# Read tool input from stdin
INPUT=$(cat)

# Extract tool name and command using jq (handles escapes, nested objects, etc.)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null || true)
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // .command // empty' 2>/dev/null || true)

# Only evaluate Bash tool calls
if [ -n "$TOOL_NAME" ] && [ "$TOOL_NAME" != "Bash" ]; then
    exit 0
fi

# If no command extracted, allow (not a Bash call or unexpected format)
if [ -z "$COMMAND" ]; then
    exit 0
fi

# Check if this is a privileged action
needs_approval() {
    case "$COMMAND" in
        *"git push"*)           return 0 ;;
        *"glab mr merge"*)      return 0 ;;
        *"glab mr approve"*)    return 0 ;;
        *"glab mr create"*)     return 0 ;;
        *"glab release create"*)return 0 ;;
        *"gh pr merge"*)        return 0 ;;
        *"gh pr create"*)       return 0 ;;
        *"gh release create"*)  return 0 ;;
        *)                      return 1 ;;
    esac
}

if ! needs_approval; then
    exit 0
fi

# Determine context (repo name from git remote or working directory)
CONTEXT="${MUX_CONTEXT:-}"
if [ -z "$CONTEXT" ]; then
    CONTEXT=$(git remote get-url origin 2>/dev/null | sed 's|.*/||;s|\.git$||' || basename "$(pwd)")
fi

# Create approval request (jq constructs safe JSON, no injection possible)
BODY=$(jq -n --arg action "$COMMAND" --arg context "$CONTEXT" \
    '{action: $action, context: $context, requester: "claude-code"}')

RESPONSE=$(curl -sf -X POST "${MUX_URL}/vault/approval" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    2>/dev/null) || {
    echo "Warning: Could not reach Mux approval API at ${MUX_URL}" >&2
    echo "DENIED: Mux approval API unreachable"
    exit 2
}

APPROVAL_ID=$(echo "$RESPONSE" | jq -r '.id // empty' 2>/dev/null || true)

if [ -z "$APPROVAL_ID" ]; then
    echo "DENIED: Failed to create approval request"
    exit 2
fi

echo "Approval requested: ${COMMAND} (${CONTEXT})" >&2
echo "Waiting for approval..." >&2

# Poll for approval
ELAPSED=0
while [ "$ELAPSED" -lt "$MAX_WAIT" ]; do
    sleep "$POLL_INTERVAL"
    ELAPSED=$((ELAPSED + POLL_INTERVAL))

    STATE=$(curl -sf "${MUX_URL}/vault/approval/${APPROVAL_ID}" 2>/dev/null \
        | jq -r '.state // empty' 2>/dev/null || true)

    case "$STATE" in
        granted)
            echo "Approved" >&2
            exit 0
            ;;
        denied)
            echo "DENIED: User denied the action"
            exit 2
            ;;
        expired)
            echo "DENIED: Approval request expired"
            exit 2
            ;;
        "")
            # curl or jq failed — retry silently
            ;;
    esac
done

echo "DENIED: Approval timed out after ${MAX_WAIT}s"
exit 2
