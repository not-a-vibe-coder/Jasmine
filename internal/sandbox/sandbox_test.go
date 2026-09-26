package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

	err := svc.HandleCallback(svc.GetSecretToken(), payload)
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
