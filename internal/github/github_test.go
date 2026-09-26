package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRepoSlug(t *testing.T) {
	svc := NewService("mock_token", "davidnzube101")

	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		hasErr    bool
	}{
		{"davidnzube101/shipp", "davidnzube101", "shipp", false},
		{"https://github.com/davidnzube101/shipp", "davidnzube101", "shipp", false},
		{"https://github.com/davidnzube101/shipp.git", "davidnzube101", "shipp", false},
		{"shipp", "davidnzube101", "shipp", false},
		{"", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := svc.ParseRepoSlug(tt.input)
		if tt.hasErr {
			if err == nil {
				t.Errorf("ParseRepoSlug(%q) expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRepoSlug(%q) unexpected error: %v", tt.input, err)
		}
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("ParseRepoSlug(%q) = (%s, %s); want (%s, %s)", tt.input, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

func TestGitHubMockEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/contents/README.md") {
			content := base64.StdEncoding.EncodeToString([]byte("# Shipp\n\nOriginal description"))
			_ = json.NewEncoder(w).Encode(FileContent{
				Name:    "README.md",
				Path:    "README.md",
				SHA:     "mock_sha_123",
				Content: content,
			})
			return
		}

		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"number":   7,
				"html_url": "https://github.com/davidnzube101/shipp/pull/7",
			})
			return
		}

		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/pulls/7/merge") {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"sha":    "abcd1234efgh5678",
				"merged": true,
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	svc := NewService("mock_token", "davidnzube101")
	svc.httpClient = server.Client()
	svc.baseURL = server.URL

	ctx := context.Background()

	// 1. Test GetFile
	fc, text, err := svc.GetFile(ctx, "davidnzube101", "shipp", "README.md", "main")
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}
	if fc.SHA != "mock_sha_123" {
		t.Errorf("expected SHA mock_sha_123, got %s", fc.SHA)
	}
	if !strings.Contains(text, "Original description") {
		t.Errorf("expected text to contain 'Original description', got %s", text)
	}

	// 2. Test CreatePullRequest
	prURL, prNum, err := svc.CreatePullRequest(ctx, "davidnzube101", "shipp", "Update README", "Details", "shipp/edit", "main")
	if err != nil {
		t.Fatalf("CreatePullRequest failed: %v", err)
	}
	if prNum != 7 || !strings.Contains(prURL, "pull/7") {
		t.Errorf("unexpected PR: num=%d, url=%s", prNum, prURL)
	}

	// 3. Test MergePullRequest
	mergeMsg, err := svc.MergePullRequest(ctx, "davidnzube101", "shipp", 7)
	if err != nil {
		t.Fatalf("MergePullRequest failed: %v", err)
	}
	if !strings.Contains(mergeMsg, "successfully merged") {
		t.Errorf("unexpected merge message: %s", mergeMsg)
	}
}
