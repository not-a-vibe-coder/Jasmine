# Shipp 🚀

A sharp, witty personal AI companion with native crypto superpowers on Solana and EVM chains.

---

## ✨ Features

1. **Intelligent Conversational AI:**
   - Powered by Groq fast inference (`qwen/qwen3.8-27b`).
   - Witty, natural, sharp, and context-aware—talks like a real member of your group chat.
   - Responds to mentions (`@Shipp0Bot`), replies, and direct messages.

2. **High-Confidence NLP & Slash Commands:**
   - **No rigid commands required!** Natural language triggers tools automatically:
     - *"What's your sol wallet address?"* ➔ outputs deposit addresses.
     - *"How much funds do you have left?"* ➔ checks live balances across Solana & EVM.
     - *"Recap what we discussed earlier"* ➔ generates a concise chat summary.
     - *"Clear context bro"* ➔ wipes recent conversation memory for a fresh start.
     - *"Send 0.01 eth to 0x... on base"* ➔ transfers crypto (owner-only).
   - Also supports direct high-speed slash commands:
     - `/wallet` or `/deposit` - View deposit addresses
     - `/balance` - Check live balances
     - `/summarize` - Recap recent conversation
     - `/clear` - Flush context memory
     - `/send <chain> <to> <amount>` - Transfer funds (Owner only)
     - `/proactive on|off` - Toggle spontaneous messages
     - `/help` - Command reference

3. **Multi-Chain Crypto Superpowers:**
   - **Solana (SVM):** Native SOL transfers, address generation, balance checks.
   - **EVM:** Base, Ethereum Mainnet, Arbitrum, BNB Smart Chain, Monad, Robinhood, Hyperliquid.
   - **Security Guardrail:** Only verified owners (`@skipp_dev`, `@shigarakiXBT`) can authorize transfers.

4. **Dual Memory Architecture:**
   - **PostgreSQL (Supabase):** Durable long-term message history and chat summaries.
   - **Redis (Aiven):** Low-latency memory caching for recent conversational context.
   - Automatic in-memory fallback for high availability.

5. **Proactive / Spontaneous Presence:**
   - Periodically drops witty thoughts, check-ins, or questions into active group chats to keep the chat lively without being spammy.

6. **Render Cloud Ready:**
   - Includes lightweight HTTP health-check server (`/healthz`) listening on `$PORT`.

---

## 🚀 Deployment on Render

### Build & Start Commands:
* **Build Command:**
  ```bash
  go build -o bin/shipp cmd/bot/main.go
  ```
* **Start Command:**
  ```bash
  ./bin/shipp
  ```

### Required Environment Variables:
Copy the variables from `.env.example` into your Render service environment settings:
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_BOT_USERNAME`
- `OWNERS_USERNAME` (e.g. `@skipp_dev,@shigarakiXBT`)
- `GROQ_API_KEY`
- `GROQ_MODEL` (`qwen/qwen3.8-27b`)
- `REDIS_URL`
- `DATABASE_URL`
- `SVM_WALLET_PUBLIC_KEY`
- `SVM_WALLET_PRIVATE_KEY`
- `EVM_WALLET_PUBLIC_KEY`
- `EVM_WALLET_PRIVATE_KEY`
- `SVM_RPC_URL`
- `BASE_RPC_URL`, `ETHEREUM_RPC_URL`, `ARBITRUM_RPC_URL`, `BNB_RPC_URL`

---

## 💡 Important Telegram Bot Setting

For Shipp to read **all** messages in your group chat (and maintain full conversational context without needing to be tagged every single time):
1. Open [@BotFather](https://t.me/BotFather) in Telegram.
2. Send `/setprivacy`.
3. Select `@Shipp0Bot`.
4. Choose **Disable**.
5. Re-add Shipp to your group chat or promote it to admin.

---

## 🧪 Running Tests

To run the complete test suite:
```bash
go test -v ./...
```