#!/usr/bin/env bash
# Point this repo at Jasmine's own GitHub remote, verify, commit, and push.
# Render (via render.yaml) redeploys automatically on every push to main.
#
#   scripts/ship.sh https://github.com/<owner>/<repo>.git ["commit message"]
set -euo pipefail

REMOTE_URL="${1:-}"
MESSAGE="${2:-feat: Jasmine, a Telegram companion with Walrus long-term memory}"
if [[ -z "$REMOTE_URL" ]]; then
  echo "usage: scripts/ship.sh <git remote url> [commit message]" >&2
  exit 1
fi

cd "$(git rev-parse --show-toplevel)"
GO="$(command -v go || echo "$HOME/.local/go/bin/go")"

echo "==> checking secrets stay local"
if git ls-files --error-unmatch .env >/dev/null 2>&1; then
  echo "refusing: .env is tracked by git" >&2
  exit 1
fi
git check-ignore -q .env || { echo "refusing: .env is not gitignored" >&2; exit 1; }

echo "==> go vet + tests"
"$GO" vet ./...
"$GO" test ./...
"$GO" build -o /dev/null ./cmd/bot

echo "==> remote"
CURRENT="$(git remote get-url origin 2>/dev/null || true)"
if [[ "$CURRENT" != "$REMOTE_URL" ]]; then
  if [[ -n "$CURRENT" ]]; then
    git remote remove shipp-upstream 2>/dev/null || true
    git remote rename origin shipp-upstream
    echo "    old origin kept as 'shipp-upstream' ($CURRENT)"
  fi
  git remote add origin "$REMOTE_URL"
fi
echo "    origin -> $REMOTE_URL"

echo "==> commit"
git add -A
if git diff --cached --quiet; then
  echo "    nothing new to commit"
else
  git commit -q -m "$MESSAGE"
  git log -1 --format='    %h %an <%ae> %s'
fi

echo "==> push"
git push -u origin HEAD:main

cat <<'NEXT'

Pushed. First time only, on Render:
  1. dashboard.render.com -> New -> Blueprint -> select this repo (it reads render.yaml)
  2. Fill the secret env vars, or run scripts/render-env.sh and use "Add from .env" to paste them
  3. Create. Health check: https://<service>.onrender.com/healthz
Every later push to main redeploys automatically.

Stop any local copy of the bot first: Telegram delivers each update to one place,
and once Render registers the webhook, local polling stops receiving messages.
NEXT
