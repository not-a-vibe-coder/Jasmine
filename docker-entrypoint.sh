#!/bin/sh
# Starts the Telegram caller next to the bot when TG_CALLER_SESSION is set, and restarts
# it if it crashes. The bot itself runs in the foreground.
set -e

if [ -n "$TG_CALLER_SESSION" ] && [ -n "$TG_API_ID" ] && [ -n "$TG_API_HASH" ]; then
  if [ -z "$CALLER_SECRET" ]; then
    CALLER_SECRET=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
    export CALLER_SECRET
  fi
  (
    while true; do
      python3 /app/caller/caller.py || echo "[Caller] exited ($?), restarting in 5s"
      sleep 5
    done
  ) &
fi

exec /app/shipp
