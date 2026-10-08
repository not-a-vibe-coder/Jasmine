#!/usr/bin/env bash
# Copy .env to the clipboard in the form Render's "Add from .env" accepts,
# dropping values Render provides itself or that only make sense locally.
# Prints only the variable names, never the values.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
[[ -f .env ]] || { echo "no .env found" >&2; exit 1; }

FILTERED="$(grep -E '^[A-Z0-9_]+=.+' .env | grep -vE '^(PORT|WEBHOOK_URL|SANDBOX_CALLBACK_URL|USE_POLLING)=')"
if command -v pbcopy >/dev/null; then
  printf '%s\n' "$FILTERED" | pbcopy
  echo "Copied $(printf '%s\n' "$FILTERED" | wc -l | tr -d ' ') variables to the clipboard:"
else
  echo "pbcopy not found; write them with: scripts/render-env.sh > render.env" >&2
  printf '%s\n' "$FILTERED"
  exit 0
fi
printf '%s\n' "$FILTERED" | cut -d= -f1 | sed 's/^/  /'
echo "Paste into Render: service -> Environment -> Add from .env"
