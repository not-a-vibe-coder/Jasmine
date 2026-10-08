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

No Claude or GPT anywhere in the chat path. Requests go through a chain, and each provider is used only when its key is set:

Groq (Qwen 3.8 27B, then GPT-OSS) → Gemini 3.5 Flash → 3.6 Flash → 3.5/3.1 Flash Lite → OpenRouter free models → Mistral → Hugging Face → native Gemini

**Focus.** Each request carries only the instructions and tools for what the conversation is about. Crypto, GitHub, sandbox, email and Moltbook rules are left out of casual chat, which halves the request (about 14k to 6.8k tokens), keeps the model on topic and answers in about 2 seconds. She sees the last 20 messages. Requests too big for Groq's free 8,000-tokens-per-minute limit go straight to Gemini, with reasoning turned off for speed. Reasoning is hidden for each provider, and a final check blocks any reply that is the model thinking out loud, whichever path produced it.

**Images:** Pollinations Z-Image Turbo and FLUX (with a free key) → Hugging Face FLUX.1-schnell → Gemini → Pollinations' keyless model.

## Talking like a person

- **Voice notes in:** she transcribes them (Groq Whisper, Gemini fallback). They wake her like text and go into her Walrus memory, so she remembers what you *said* as well as what you typed.
- **Voice notes out:** she answers a voice note with one. She uses Groq Orpheus, which can laugh and sigh, once its terms are accepted, or a Microsoft neural voice (`VOICE_NAME`, e.g. Nigerian English `en-NG-EzinneNeural`).
- **Reactions, stickers, GIFs:** she taps an emoji reaction, sends a sticker from public sets, or searches Giphy. When that says it all, she sends no extra text.

## Emotional support

She reads the mood under a message, names feelings specifically, listens before advising, and follows up later using her Walrus memory ("did you manage to sleep after yesterday?"). She is a caring presence, not a therapist: if someone talks about self-harm she stays with them and points them to a real person (in Nigeria: 112 or the free 24/7 MANI line 0800 800 2000).

## Reminders

"jasmine remind me by 3pm lagos time to fetch water" schedules a reminder; at 3pm she tags you in the same chat with a message she writes herself. She also understands "in 20 mins", "every morning" (daily/weekly repeats), reminding someone else, and listing or cancelling reminders. They are stored in Postgres, and missed ones (while Render slept) are sent on wake. While any are pending she pings her own `/healthz` every 10 minutes so the free plan doesn't put her to sleep. `DEFAULT_TIMEZONE` sets the zone used when nobody names one (default `Africa/Lagos`).

## Calls

"jasmine call me" rings you and she holds a real spoken conversation: she talks (same voice as her voice notes), listens, transcribes, answers, and says goodbye when you're done. What you say on the call goes into her Walrus memory. Reminders can arrive as calls too ("call me at 6am to wake me up"); if you don't pick up, she texts the reminder instead.

- **Telegram calls** come from a second, normal Telegram account, because bots can't place calls. `caller/caller.py` (py-tgcalls) runs next to the bot in the same container. Create the account, get `TG_API_ID`/`TG_API_HASH` at my.telegram.org, then run `uv run --with telethon python scripts/tg-caller-login.py` once to get `TG_CALLER_SESSION`.
- **Phone calls** go through Twilio (`TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `TWILIO_FROM_NUMBER`). People give her their number in a DM; it is stored in Postgres, never shown in chat. Twilio webhooks are signature-checked.
- Only the owner can ask her to call someone else; everyone else gets 3 calls a day.

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
