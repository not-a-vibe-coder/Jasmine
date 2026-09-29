package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Task struct {
	ID           string
	ChatID       int64
	ThreadID     int
	ReplyToMsgID int
	Command      string
	Prompt       string
	Repo         string
	IsPrivate    bool
	StartTime    time.Time
	Done         chan struct{}
}

type CallbackPayload struct {
	TaskID          string `json:"task_id"`
	ChatID          int64  `json:"chat_id"`
	ExitCode        int    `json:"exit_code"`
	DurationSeconds int    `json:"duration_seconds"`
	Output          string `json:"output"`
}

var (
	// EVM private key: 64 hex characters (optionally prefixed with 0x)
	reEVMPrivateKey = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(?:0x)?[a-f0-9]{64}(?:[^a-zA-Z0-9]|$)`)

	// Standard PEM Private Key blocks
	rePEMPrivateKey = regexp.MustCompile(`(?i)-----BEGIN (?:[A-Z0-9_-]+ )?PRIVATE KEY-----`)

	// Common API keys and tokens (GitHub, Stripe, Slack, AWS, Resend, Google)
	reAPIKeyPrefixes = regexp.MustCompile(`(?i)(?:ghp_[a-zA-Z0-9]{30,}|github_pat_[a-zA-Z0-9_]{30,}|sk_live_[a-zA-Z0-9]{20,}|sk_test_[a-zA-Z0-9]{20,}|xox[baprs]-[0-9a-zA-Z]{10,}|AKIA[0-9A-Z]{16}|re_[a-zA-Z0-9_]{20,}|AIza[0-9A-Za-z-_]{35})`)

	// Auth headers and bearer tokens
	reAuthHeader = regexp.MustCompile(`(?i)(?:authorization:\s*bearer|bearer\s+[a-zA-Z0-9_\-\.]{25,})`)

	// Sensitive environment variables and files
	reEnvSecret = regexp.MustCompile(`(?i)(?:\.env\b|PRIVATE_KEY|SECRET_KEY|MNEMONIC|SEED_PHRASE|API_KEY|PASSWORD|ACCESS_TOKEN)`)
)

// IsSensitiveCommand inspects a command string and original prompt to detect whether it contains
// private keys, credentials, tokens, or secret configurations requiring the private sandbox.
func IsSensitiveCommand(command, prompt string) bool {
	combined := command + "\n" + prompt
	if rePEMPrivateKey.MatchString(combined) {
		return true
	}
	if reAPIKeyPrefixes.MatchString(combined) {
		return true
	}
	if reAuthHeader.MatchString(combined) {
		return true
	}
	if reEnvSecret.MatchString(combined) {
		return true
	}
	if reEVMPrivateKey.MatchString(command) {
		matches := reEVMPrivateKey.FindAllString(command, -1)
		for _, m := range matches {
			trimmed := strings.Trim(m, " \t\r\n;\"'`")
			trimmed = strings.TrimPrefix(trimmed, "0x")
			if trimmed != strings.Repeat("0", 64) {
				return true
			}
		}
	}

	lower := strings.ToLower(combined)
	sensitivePhrases := []string{
		"private key", "privkey", "secret key", "seed phrase", "mnemonic",
		"api token", "api key", "access token", "bearer token", "secret_key",
		"private_sandbox", "sandbox-private",
	}
	for _, phrase := range sensitivePhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	return false
}

type Service struct {
	githubPAT   string
	publicRepo  string
	privateRepo string
	callbackURL string
	secretToken string
	httpClient  *http.Client

	mu         sync.RWMutex
	tasks      map[string]*Task
	onComplete func(payload CallbackPayload)
	onTimeout  func(task *Task)
}

func NewService(githubPAT, defaultRepo, callbackURL string) *Service {
	// Generate a stable random secret for this bot instance
	secretBytes := make([]byte, 16)
	_, _ = rand.Read(secretBytes)
	secret := hex.EncodeToString(secretBytes)

	publicRepo := defaultRepo
	if publicRepo == "" {
		publicRepo = "ShippZero/sandbox"
	}
	privateRepo := "ShippZero/sandbox-private"

	if callbackURL == "" {
		callbackURL = "https://bot.davidnzube.xyz/api/sandbox/callback"
	}

	return &Service{
		githubPAT:   githubPAT,
		publicRepo:  publicRepo,
		privateRepo: privateRepo,
		callbackURL: callbackURL,
		secretToken: secret,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		tasks:       make(map[string]*Task),
	}
}

func (s *Service) GetPublicRepo() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publicRepo
}

func (s *Service) GetPrivateRepo() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.privateRepo
}

func (s *Service) SetRepos(publicRepo, privateRepo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if publicRepo != "" {
		s.publicRepo = publicRepo
	}
	if privateRepo != "" {
		s.privateRepo = privateRepo
	}
}

// ResolveRepo decides whether to target publicRepo or privateRepo based on explicit repo argument or code sensitivity
func (s *Service) ResolveRepo(repo, command, prompt string) (string, bool) {
	cleaned := strings.ToLower(strings.TrimSpace(repo))
	if cleaned == "private" || cleaned == "sandbox-private" || cleaned == "shippzero/sandbox-private" {
		return s.GetPrivateRepo(), true
	}
	if cleaned == "public" || cleaned == "sandbox" || cleaned == "shippzero/sandbox" {
		return s.GetPublicRepo(), false
	}
	if cleaned != "" {
		isPriv := strings.EqualFold(cleaned, s.GetPrivateRepo())
		return repo, isPriv
	}

	// Auto-decide based on code sensitivity
	if IsSensitiveCommand(command, prompt) {
		return s.GetPrivateRepo(), true
	}
	return s.GetPublicRepo(), false
}

func (s *Service) SetHandlers(onComplete func(payload CallbackPayload), onTimeout func(task *Task)) {
	s.onComplete = onComplete
	s.onTimeout = onTimeout
}

func (s *Service) GetSecretToken() string {
	return s.secretToken
}

func (s *Service) GetTask(taskID string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[taskID]
}

// Dispatch triggers a GitHub Actions ephemeral runner workflow
func (s *Service) Dispatch(ctx context.Context, chatID int64, command, repo string) (string, error) {
	return s.DispatchWithPrompt(ctx, chatID, 0, 0, command, repo, "")
}

// DispatchWithThread triggers a GitHub Actions ephemeral runner workflow with thread and reply info
func (s *Service) DispatchWithThread(ctx context.Context, chatID int64, threadID, replyToMsgID int, command, repo string) (string, error) {
	return s.DispatchWithPrompt(ctx, chatID, threadID, replyToMsgID, command, repo, "")
}

// DispatchWithPrompt triggers a GitHub Actions ephemeral runner workflow with thread, reply, and original prompt info
func (s *Service) DispatchWithPrompt(ctx context.Context, chatID int64, threadID, replyToMsgID int, command, repo, prompt string) (string, error) {
	if s.githubPAT == "" {
		return "", fmt.Errorf("GITHUB_PAT is not configured")
	}

	targetRepo, isPrivate := s.ResolveRepo(repo, command, prompt)

	taskIDBytes := make([]byte, 8)
	_, _ = rand.Read(taskIDBytes)
	prefix := "task_"
	if prompt == "autonomous sandbox experiment" {
		prefix = "auto_task_"
	}
	taskID := fmt.Sprintf("%s%x", prefix, taskIDBytes)

	task := &Task{
		ID:           taskID,
		ChatID:       chatID,
		ThreadID:     threadID,
		ReplyToMsgID: replyToMsgID,
		Command:      command,
		Prompt:       prompt,
		Repo:         targetRepo,
		IsPrivate:    isPrivate,
		StartTime:    time.Now(),
		Done:         make(chan struct{}),
	}

	s.mu.Lock()
	s.tasks[taskID] = task
	s.mu.Unlock()

	log.Printf("[Sandbox] Dispatching task %s to %s (private=%v, command: %q)", taskID, targetRepo, isPrivate, command)

	// Dispatch workflow via GitHub REST API
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/sandbox.yml/dispatches", targetRepo)
	payload := map[string]interface{}{
		"ref": "main",
		"inputs": map[string]string{
			"task_id":      taskID,
			"chat_id":      fmt.Sprintf("%d", chatID),
			"command":      command,
			"callback_url": s.callbackURL,
			"auth_token":   s.secretToken,
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode dispatch payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+s.githubPAT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Shipp-Sandbox-Dispatcher/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.cleanupTask(taskID)
		return "", fmt.Errorf("github api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		s.cleanupTask(taskID)
		return "", fmt.Errorf("github dispatch rejected (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Start 6-minute timeout watchdog
	go func() {
		select {
		case <-task.Done:
			// Task completed normally
		case <-time.After(6 * time.Minute):
			s.mu.Lock()
			_, exists := s.tasks[taskID]
			if exists {
				delete(s.tasks, taskID)
			}
			s.mu.Unlock()

			if exists && s.onTimeout != nil {
				s.onTimeout(task)
			}
		}
	}()

	return taskID, nil
}

// HandleCallback processes the HTTP callback payload from the GitHub Actions runner
func (s *Service) HandleCallback(authToken string, payload CallbackPayload) error {
	if authToken != s.secretToken {
		return fmt.Errorf("unauthorized callback secret")
	}

	s.mu.Lock()
	task, exists := s.tasks[payload.TaskID]
	if exists {
		delete(s.tasks, payload.TaskID)
	}
	s.mu.Unlock()

	if !exists {
		log.Printf("[Sandbox] Received callback for unknown or expired task: %s", payload.TaskID)
		return nil
	}

	close(task.Done)

	if s.onComplete != nil {
		s.onComplete(payload)
	}

	return nil
}

func (s *Service) cleanupTask(taskID string) {
	s.mu.Lock()
	delete(s.tasks, taskID)
	s.mu.Unlock()
}

// CallbackHTTPHandler handles incoming POST webhook callbacks from GitHub Actions runners
func (s *Service) CallbackHTTPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	authToken := r.Header.Get("X-Sandbox-Token")
	if authToken == "" {
		authToken = r.Header.Get("X-Sandbox-Auth")
	}
	if authToken == "" {
		authToken = r.URL.Query().Get("token")
	}

	var payload CallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("[Sandbox] Invalid callback JSON: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := s.HandleCallback(authToken, payload); err != nil {
		log.Printf("[Sandbox] Callback unauthorized: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

