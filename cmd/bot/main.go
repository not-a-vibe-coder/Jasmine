package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"shipp/internal/ai"
	"shipp/internal/bot"
	"shipp/internal/calls"
	"shipp/internal/config"
	"shipp/internal/crypto"
	"shipp/internal/domain"
	"shipp/internal/email"
	"shipp/internal/github"
	"shipp/internal/imagegen"
	"shipp/internal/memory"
	"shipp/internal/moltbook"
	"shipp/internal/reminders"
	"shipp/internal/price"
	"shipp/internal/sandbox"
	"shipp/internal/search"
	"shipp/internal/server"
	"shipp/internal/token"
	"shipp/internal/vision"
	"shipp/internal/voice"
	"shipp/internal/walmem"
	"shipp/internal/xhandle"
)

func main() {
	log.Printf("-----------------------------------------")
	log.Printf("🚀 Starting Jasmine Telegram Bot")
	log.Printf("-----------------------------------------")

	// 1. Load Config
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("[Main] Configuration error: %v", err)
	}
	log.Printf("[Main] Config loaded successfully (Owners: %v)", cfg.Owners)

	// 2. Initialize Memory Store (Postgres + Redis hybrid with in-memory fallback)
	memStore, err := memory.NewHybridStore(cfg.DatabaseURL, cfg.RedisURL)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize memory store: %v", err)
	}
	defer memStore.Close()

	// 3. Initialize Crypto Service
	cryptoSvc, err := crypto.NewService(
		cfg.SVMWalletPublicKey,
		cfg.SVMWalletPrivateKey,
		cfg.SVMRPCURL,
		cfg.SVMFallbackRPC,
		cfg.EVMWalletPublicKey,
		cfg.EVMWalletPrivateKey,
		cfg.BaseRPCURL,
		cfg.EthereumRPCURL,
		cfg.ArbitrumRPCURL,
		cfg.BnbRPCURL,
		cfg.RobinhoodRPCURL,
	)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize crypto service: %v", err)
	}
	svmAddr, evmAddr := cryptoSvc.GetAddresses()
	log.Printf("[Main] Crypto Service initialized | SVM: %s | EVM: %s", svmAddr, evmAddr)

	// 4. Initialize AI Client & Search Service
	aiClient := ai.NewClient(cfg.GroqAPIKey, cfg.GroqModel, cfg.GeminiAPIKey, cfg.Owners)
	// Fallback chain after the Groq pool. Each provider is skipped when its key is empty.
	// Gemini via its OpenAI-compatible endpoint: generous free limits and solid tool calling.
	aiClient.AddProvider(ai.Provider{Name: "gemini", URL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		APIKey: cfg.GeminiAPIKey, Models: modelList(os.Getenv("GEMINI_CHAT_MODELS"), "gemini-3.5-flash,gemini-3.6-flash,gemini-flash-latest,gemini-3.5-flash-lite,gemini-3.1-flash-lite")})
	aiClient.AddProvider(ai.Provider{Name: "cerebras", URL: "https://api.cerebras.ai/v1/chat/completions",
		APIKey: cfg.CerebrasAPIKey, Models: modelList(cfg.CerebrasModels, "qwen-3.8-27b,gpt-oss-120b")})
	aiClient.AddProvider(ai.Provider{Name: "openrouter", URL: "https://openrouter.ai/api/v1/chat/completions",
		APIKey: cfg.OpenRouterAPIKey, Models: modelList(cfg.OpenRouterModels, "nvidia/nemotron-3-super-120b-a12b:free,google/gemma-4-31b-it:free,nvidia/nemotron-3.5-lightning:free")})
	aiClient.AddProvider(ai.Provider{Name: "mistral", URL: "https://api.mistral.ai/v1/chat/completions",
		APIKey: cfg.MistralAPIKey, Models: modelList(cfg.MistralModels, "mistral-small-latest")})
	aiClient.AddProvider(ai.Provider{Name: "huggingface", URL: "https://router.huggingface.co/v1/chat/completions",
		APIKey: cfg.HuggingFaceAPIKey, Models: modelList(cfg.HuggingFaceModels, "openai/gpt-oss-120b")})
	log.Printf("[Main] AI fallback chain: %s", strings.Join(aiClient.ProviderNames(), " -> "))

	searchSvc := search.NewService(cfg.TavilyAPIKey)
	log.Printf("[Main] Web Search Service initialized (DuckDuckGo + Wikipedia active)")

	tokenSvc := token.NewService(cfg.CodexIOAPIKey)
	log.Printf("[Main] Token CA Analytics Service initialized (DexScreener + Codex.io active)")

	priceSvc := price.NewService()
	log.Printf("[Main] Price Service initialized (CoinGecko Simple Price + Cache active)")

	visionSvc := vision.NewService(cfg.GeminiAPIKey)
	log.Printf("[Main] Vision Service initialized (Gemini Flash Multimodal active)")

	githubSvc := github.NewService(cfg.GithubPAT, cfg.GithubUsername, cfg.PersonalEmail)
	log.Printf("[Main] GitHub Service initialized for user @%s (Email: %s)", cfg.GithubUsername, cfg.PersonalEmail)

	emailSvc := email.NewService(cfg.ResendAPIKey, cfg.ResendFromEmail, cfg.PersonalEmail)
	if emailSvc.IsConfigured() {
		log.Printf("[Main] Resend Email Service initialized (From: %s | Reply-To: %s)", cfg.ResendFromEmail, cfg.PersonalEmail)
	} else {
		log.Printf("[Main] Resend Email Service not configured (RESEND_API_KEY missing)")
	}

	// 5. Initialize Ephemeral Sandbox Service (Blink Compute pattern)
	sandboxSvc := sandbox.NewService(cfg.GithubPAT, cfg.SandboxRepo, cfg.SandboxCallbackURL)
	if cfg.SandboxPrivateRepo != "" {
		sandboxSvc.SetRepos(sandboxSvc.GetPublicRepo(), cfg.SandboxPrivateRepo)
	}
	log.Printf("[Main] Sandbox Service initialized (public: %s, private: %s, callback: %s)", sandboxSvc.GetPublicRepo(), sandboxSvc.GetPrivateRepo(), cfg.SandboxCallbackURL)

	// 6. Initialize Vercel Domain Registrar Service
	domainSvc := domain.NewService(cfg.VercelToken)
	log.Printf("[Main] Vercel Domain Registrar Service initialized")

	// 7. Initialize X Handle Availability Service
	xhandleSvc := xhandle.NewService()
	log.Printf("[Main] X Handle Availability Service initialized")

	// 8. Initialize Moltbook AI Social Network Service
	moltbookSvc := moltbook.NewClient(cfg.MoltbookAPIKey)
	if moltbookSvc.IsConfigured() {
		log.Printf("[Main] Moltbook Service initialized")
	} else {
		log.Printf("[Main] Moltbook Service not configured (MOLTBOOK_API_KEY missing)")
	}

	// 9. Initialize Telegram Bot
	tgBot, err := bot.NewBot(cfg, aiClient, memStore, cryptoSvc, searchSvc, tokenSvc, priceSvc, visionSvc, githubSvc, emailSvc, sandboxSvc, domainSvc, xhandleSvc, moltbookSvc)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize Telegram Bot: %v", err)
	}

	// 10. Walrus Memory: long-term, encrypted, user-owned memory on Walrus mainnet
	walmemClient, err := walmem.NewClient(cfg.MemwalServerURL, cfg.MemwalAccountID, cfg.MemwalDelegateKey)
	if err != nil {
		log.Fatalf("[Main] Walrus Memory config error: %v", err)
	}
	if walmemClient != nil {
		tgBot.SetWalrusMemory(walmemClient)
		log.Printf("[Main] Walrus Memory active (account %s)", cfg.MemwalAccountID)
	} else {
		log.Printf("[Main] Walrus Memory not configured (MEMWAL_ACCOUNT_ID / MEMWAL_DELEGATE_KEY missing)")
	}

	// 11. Image generation: Pollinations (no key needed) with Gemini fallback
	imageModels := modelList(cfg.PollinationsModels, strings.Join(imagegen.DefaultPollinationsModels, ","))
	imageSvc := imagegen.NewService(cfg.PollinationsAPIKey, cfg.GeminiAPIKey, cfg.GeminiImageModel, imageModels...)
	if cfg.HuggingFaceAPIKey != "" {
		imageSvc.SetHuggingFace(cfg.HuggingFaceAPIKey, cfg.HFImageModel)
	}
	tgBot.SetImageGen(imageSvc)

	// 12. Voice notes: Groq Whisper in; Groq Orpheus (if enabled) or Edge neural voices out
	tgBot.SetVoice(voice.NewService(cfg.GroqAPIKey, cfg.GeminiAPIKey, cfg.VoiceName, cfg.OrpheusVoice))

	if tz := os.Getenv("DEFAULT_TIMEZONE"); tz != "" {
		reminders.DefaultZone = tz
	}
	// Voice calls: Twilio for phone numbers, a companion Telegram account for Telegram calls.
	publicURL := os.Getenv("RENDER_EXTERNAL_URL")
	if publicURL == "" {
		publicURL = strings.TrimSuffix(cfg.WebhookURL, "/webhook")
	}
	callMgr := calls.NewManager(tgBot, publicURL)
	callMgr.Twilio = calls.NewTwilio(os.Getenv("TWILIO_ACCOUNT_SID"), os.Getenv("TWILIO_AUTH_TOKEN"),
		os.Getenv("TWILIO_FROM_NUMBER"), os.Getenv("TWILIO_SPEECH_LANG"))
	if os.Getenv("TG_CALLER_SESSION") != "" {
		callMgr.Telegram = calls.NewTelegramCaller(envOr("CALLER_URL", "http://127.0.0.1:8091"), os.Getenv("CALLER_SECRET"), os.Getenv("CALLER_CLIP_DIR"))
	}
	phonebook, err := calls.NewPhonebook(context.Background(), memStore.GetDB())
	if err != nil {
		log.Printf("[Main] Phonebook unavailable: %v", err)
	}
	tgBot.SetCalls(callMgr, phonebook)
	phoneOK, tgOK := callMgr.Available()
	log.Printf("[Main] Calls: phone (Twilio) %v, Telegram %v", phoneOK, tgOK)

	if remStore, err := reminders.New(context.Background(), memStore.GetDB()); err != nil {
		log.Printf("[Main] Reminders unavailable: %v", err)
	} else {
		tgBot.SetReminders(remStore)
		log.Printf("[Main] Reminders enabled (default timezone %s)", reminders.DefaultZone)
	}
	if cfg.PollinationsAPIKey != "" {
		log.Printf("[Main] Image generation: Pollinations %s", strings.Join(imageModels, " -> "))
	} else {
		log.Printf("[Main] Image generation: keyless Pollinations only (set POLLINATIONS_API_KEY for better models)")
	}

	// 7. Initialize & Start HTTP Server for Render Healthchecks & Webhooks
	httpServer := server.NewServer(cfg.Port, memStore.GetDB(), memStore.GetRedis())
	httpServer.RegisterHandler("/webhook", tgBot.WebhookHandler)
	httpServer.RegisterHandler("/api/sandbox/callback", sandboxSvc.CallbackHTTPHandler)
	httpServer.RegisterHandler("/calls/audio/", callMgr.AudioHandler)
	httpServer.RegisterHandler("/calls/twilio/", callMgr.TwilioHandler)
	httpServer.RegisterHandler("/calls/tg/event", callMgr.TelegramEventHandler)
	go func() {
		if err := httpServer.Start(); err != nil {
			log.Printf("[Main] HTTP server exited: %v", err)
		}
	}()

	// 7. Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run Bot polling in goroutine
	go func() {
		if err := tgBot.Start(ctx); err != nil {
			log.Fatalf("[Main] Bot error: %v", err)
		}
	}()

	// 8. Wait for OS termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	log.Printf("[Main] Received signal %v, initiating graceful shutdown...", sig)
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Stop(shutdownCtx)

	log.Printf("[Main] Jasmine gracefully stopped. Goodbye!")
}

// modelList parses a comma-separated model override, falling back to defaults.
func modelList(override, defaults string) []string {
	raw := override
	if strings.TrimSpace(raw) == "" {
		raw = defaults
	}
	var out []string
	for _, m := range strings.Split(raw, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
