# Jasmine

A Telegram companion that remembers people across conversations, across users, and across devices. Built for Walrus Session 8: Chatbots That Remember.

Talk to her in a group by saying "jasmine" or tagging `@JasmineMemBot`, or DM her. Tell her something once and she still knows it next week, in a different chat, on a different phone.

## How memory works

Jasmine's long-term memory is [Walrus Memory](https://docs.wal.app/walrus-memory/llms.txt) on **mainnet**. Each memory is embedded, Seal-encrypted and stored as a blob on Walrus, owned by an on-chain account on Sui.

Every message goes through three steps:

1. **Recall.** Before replying, Jasmine runs a semantic search over the speaker's memory (and, in groups, the group's shared memory). Relevant facts go into the system prompt. Anything past a cosine distance of 0.8 is dropped as unrelated.
2. **Reply.** The model answers using those memories naturally, the way a friend would.
3. **Learn.** After replying, the message goes to the relayer's `/api/analyze`, which extracts lasting facts and writes each one to Walrus. This runs in the background, so replies are never slowed down.

| Scope | Namespace | Gives you |
|---|---|---|
| Person | `tg-user-<telegram id>` | Remembers you in every group and DM, on every device |
| Group | `tg-group-<chat id>` | Shared group context: anyone can ask about what someone else told her |

Short-term context (the last few messages) stays in Postgres/Redis. Walrus holds everything long-term.

### Native Go client

The official SDK is TypeScript, so `internal/walmem` talks to the relayer directly. Each request is signed with the account's Ed25519 delegate key over:

```
{timestamp}.{method}.{path}.{sha256(body)}.{nonce}.{account_id}
```

and sent as `x-public-key` / `x-signature` / `x-timestamp` / `x-nonce` / `x-account-id` headers. There's no Node sidecar.

### Commands

| Command | What it does |
|---|---|
| `/memory` | What Jasmine remembers about you, with a walruscan.com link to each blob |
| `/remember <fact>` | Save a fact now and get back its Walrus blob link |
| `/forget` | Delete everything she remembers about you |
| `/imagine <prompt>` | Generate an image (also works by asking "jasmine, draw ...") |
| `/whoami` | Your Telegram ID and whether you're an owner |

## Models

Jasmine runs on open models, with no Claude or GPT in the chat path. Requests fall through a chain so one provider's rate limit doesn't take her down:

Groq (Qwen 3.8 27B, then GPT-OSS) → Cerebras → OpenRouter free models → Mistral → Hugging Face → Gemini Flash

A provider is used only when its key is set. Images come from Pollinations (Z-Image Turbo, then FLUX models, with a free key), falling back to Gemini, then to Pollinations' keyless model. Reasoning is hidden per provider, and any reply that is the model thinking out loud is discarded and retried on the next model.

## Other abilities

Web search and page reading, token analytics, Solana and EVM wallets (owner-only sends), GitHub actions, a private GitHub Actions sandbox for running code, document and image understanding, DMs, and spontaneous group check-ins.

Owners are matched by permanent Telegram user ID (`OWNER_IDS`) or exact `@username`, never by display name. Webhook updates must carry Telegram's secret token, and each sandbox run gets a one-time callback token.

## Run it

```bash
cp .env.example .env            # fill in the bot token, Walrus account, and AI keys
go run ./cmd/walmem-check roundtrip   # checks the Walrus credentials with a live write and recall
go run ./cmd/genwallet          # optional: fresh wallets written straight to .env
go run ./cmd/bot
```

## Deploy to Render

```bash
scripts/ship.sh https://github.com/<owner>/<repo>.git   # vet, test, commit, push
scripts/render-env.sh                                    # copies .env for Render's "Add from .env"
```

The first time, create the service on Render: New → Blueprint, then pick the repo. `render.yaml` sets up a Docker web service with a `/healthz` check. Jasmine builds her webhook and sandbox callback URLs from `RENDER_EXTERNAL_URL`, so there are no URLs to configure. Every push to `main` redeploys.

Telegram updates are only accepted with the secret token registered at startup. To run anywhere else, set `WEBHOOK_URL=https://<host>/webhook`, or leave it empty to use long polling.

```bash
go test ./...
```
