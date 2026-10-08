package sandbox

import (
	"strings"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsSensitiveCommand(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		prompt   string
		wantPriv bool
	}{
		{
			name:     "EVM private key in command",
			command:  "echo ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
			wantPriv: true,
		},
		{
			name:     "GitHub PAT prefix",
			command:  "curl -H 'Authorization: token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ01234567'",
			wantPriv: true,
		},
		{
			name:     "Stripe secret key",
			command:  "STRIPE_KEY=sk_live_ABCDEFGHIJKLMNOPQRSTUV go run main.go",
			wantPriv: true,
		},
		{
			name:     "PEM private key header",
			command:  "-----BEGIN RSA PRIVATE KEY-----",
			wantPriv: true,
		},
		{
			name:     "bearer token",
			command:  "curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig'",
			wantPriv: true,
		},
		{
			name:     ".env file reference",
			command:  "cat .env",
			wantPriv: true,
		},
		{
			name:     "sensitive phrase in prompt",
			command:  "go run main.go",
			prompt:   "use my private key stored in the config",
			wantPriv: true,
		},
		{
			name:     "clean math script",
			command:  "python3 -c 'print(sum(range(1,101)))'",
			wantPriv: false,
		},
		{
			name:     "go version check",
			command:  "go version",
			wantPriv: false,
		},
		{
			name:     "all zeros EVM (not a real key)",
			command:  "echo 0000000000000000000000000000000000000000000000000000000000000000",
			wantPriv: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsSensitiveCommand(tc.command, tc.prompt)
			if got != tc.wantPriv {
				t.Errorf("IsSensitiveCommand(%q, %q) = %v, want %v", tc.command, tc.prompt, got, tc.wantPriv)
			}
		})
	}
}

func TestResolveRepo(t *testing.T) {
	svc := NewService("mock_pat", "ShippZero/sandbox", "http://localhost/callback")
	svc.SetRepos("", "ShippZero/sandbox-private")

	cases := []struct {
		name     string
		repo     string
		command  string
		prompt   string
		wantRepo string
		wantPriv bool
	}{
		{
			name:     "explicit private",
			repo:     "private",
			command:  "echo hello",
			wantRepo: "ShippZero/sandbox-private",
			wantPriv: true,
		},
		{
			name:     "explicit public",
			repo:     "public",
			command:  "echo hello",
			wantRepo: "ShippZero/sandbox",
			wantPriv: false,
		},
		{
			name:     "auto-detect clean code goes public",
			repo:     "",
			command:  "go version",
			wantRepo: "ShippZero/sandbox",
			wantPriv: false,
		},
		{
			name:     "auto-detect sensitive goes private",
			repo:     "",
			command:  "echo ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
			wantRepo: "ShippZero/sandbox-private",
			wantPriv: true,
		},
		{
			name:     "full private repo name",
			repo:     "ShippZero/sandbox-private",
			command:  "echo hello",
			wantRepo: "ShippZero/sandbox-private",
			wantPriv: true,
		},
		{
			name:     "full public repo name",
			repo:     "ShippZero/sandbox",
			command:  "echo hello",
			wantRepo: "ShippZero/sandbox",
			wantPriv: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRepo, gotPriv := svc.ResolveRepo(tc.repo, tc.command, tc.prompt)
			if gotRepo != tc.wantRepo {
				t.Errorf("ResolveRepo repo: got %q, want %q", gotRepo, tc.wantRepo)
			}
			if gotPriv != tc.wantPriv {
				t.Errorf("ResolveRepo isPrivate: got %v, want %v", gotPriv, tc.wantPriv)
			}
		})
	}
}

func TestSandboxCallbackAndWatchdog(t *testing.T) {
	// Mock GitHub API server
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mockGH.Close()

	svc := NewService("mock_pat", "DavidNzube101/shipp", "http://localhost:8080/callback")
	// Use mock GH server for testing
	svc.httpClient = mockGH.Client()

	var completedPayload CallbackPayload
	doneChan := make(chan struct{})

	svc.SetHandlers(func(payload CallbackPayload) {
		completedPayload = payload
		close(doneChan)
	}, func(task *Task) {
		// timeout
	})

	// Dispatch simulated task
	taskID := "task_test123"
	task := &Task{
		ID:        taskID,
		ChatID:    12345,
		Command:   "go test ./...",
		Token:     "task-secret",
		StartTime: time.Now(),
		Done:      make(chan struct{}),
	}
	svc.mu.Lock()
	svc.tasks[taskID] = task
	svc.mu.Unlock()

	// Simulate runner sending callback
	payload := CallbackPayload{
		TaskID:          taskID,
		ChatID:          12345,
		ExitCode:        0,
		DurationSeconds: 12,
		Output:          "PASS: all tests green",
	}

	if err := svc.HandleCallback("wrong-token", payload); err == nil {
		t.Fatal("expected a forged callback token to be rejected")
	}
	err := svc.HandleCallback("task-secret", payload)
	if err != nil {
		t.Fatalf("unexpected callback error: %v", err)
	}

	select {
	case <-doneChan:
		if completedPayload.ExitCode != 0 || completedPayload.Output != "PASS: all tests green" {
			t.Errorf("payload mismatch: %+v", completedPayload)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("callback was not delivered in time")
	}
}

func TestSecretCommandsRefusedWithoutPrivateRepo(t *testing.T) {
	svc := NewService("mock_pat", "owner/public-sandbox", "http://localhost/callback")
	_, err := svc.DispatchWithPrompt(t.Context(), 1, 0, 0, "echo $PRIVATE_KEY", "", "")
	if err == nil || !strings.Contains(err.Error(), "private sandbox") {
		t.Fatalf("expected refusal for secret command with no private repo, got %v", err)
	}
}
