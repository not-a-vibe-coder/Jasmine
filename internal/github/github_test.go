package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRepoSlug(t *testing.T) {
	svc := NewService("mock_token", "davidnzube101", "shipp@bot.internal")

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
	var receivedCommitPayload map[string]interface{}

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

		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/contents/README.md") {
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(bodyBytes, &receivedCommitPayload)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"commit": map[string]string{"sha": "c123456"},
				"content": map[string]string{"html_url": "https://github.com/davidnzube101/shipp/blob/main/README.md"},
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

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 1,
				"workflow_runs": []WorkflowRun{
					{
						ID:         101,
						Name:       "CI",
						HeadBranch: "main",
						HeadSHA:    "abc1234567",
						Status:     "completed",
						Conclusion: "success",
						HTMLURL:    "https://github.com/davidnzube101/shipp/actions/runs/101",
						Actor: struct {
							Login string `json:"login"`
						}{Login: "skipp_dev"},
					},
				},
			})
			return
		}

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/releases") {
			now := time.Now()
			_ = json.NewEncoder(w).Encode([]Release{
				{
					ID:          201,
					TagName:     "v1.0.0",
					Name:        "Genesis Release",
					Body:        "First release of Shipp bot",
					HTMLURL:     "https://github.com/davidnzube101/shipp/releases/tag/v1.0.0",
					PublishedAt: &now,
				},
			})
			return
		}

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits") {
			_ = json.NewEncoder(w).Encode([]CommitInfo{
				{
					SHA:     "d640e04123456",
					HTMLURL: "https://github.com/davidnzube101/shipp/commit/d640e04",
				},
			})
			return
		}

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues") {
			_ = json.NewEncoder(w).Encode([]IssueInfo{
				{
					Number:  3,
					Title:   "Support QR code links",
					State:   "open",
					HTMLURL: "https://github.com/davidnzube101/shipp/issues/3",
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	svc := NewService("mock_token", "davidnzube101", "shipp@bot.internal")
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

	// 2. Test CommitFileWithOptions with Default Shipp credentials
	_, err = svc.CommitFileWithOptions(ctx, "davidnzube101", "shipp", "README.md", CommitOptions{
		Branch:  "main",
		Message: "update readme",
		Content: "# Updated",
	})
	if err != nil {
		t.Fatalf("CommitFileWithOptions failed: %v", err)
	}
	authorMap, ok := receivedCommitPayload["author"].(map[string]interface{})
	if !ok || authorMap["name"] != "Shipp" || authorMap["email"] != "shipp@bot.internal" {
		t.Errorf("expected author to be name=Shipp email=shipp@bot.internal, got: %v", authorMap)
	}

	// 3. Test CommitFileWithOptions with Custom Credentials
	_, err = svc.CommitFileWithOptions(ctx, "davidnzube101", "shipp", "README.md", CommitOptions{
		Branch:      "main",
		Message:     "custom update",
		Content:     "# Custom",
		AuthorName:  "Alice Dev",
		AuthorEmail: "alice@company.com",
		CustomPAT:   "custom_pat_999",
	})
	if err != nil {
		t.Fatalf("CommitFileWithOptions custom failed: %v", err)
	}
	authorMapCustom := receivedCommitPayload["author"].(map[string]interface{})
	if authorMapCustom["name"] != "Alice Dev" || authorMapCustom["email"] != "alice@company.com" {
		t.Errorf("expected custom author Alice Dev, got: %v", authorMapCustom)
	}

	// 4. Test CreatePullRequest
	prURL, prNum, err := svc.CreatePullRequest(ctx, "davidnzube101", "shipp", "Update README", "Details", "shipp/edit", "main")
	if err != nil {
		t.Fatalf("CreatePullRequest failed: %v", err)
	}
	if prNum != 7 || !strings.Contains(prURL, "pull/7") {
		t.Errorf("unexpected PR: num=%d, url=%s", prNum, prURL)
	}

	// 5. Test MergePullRequest
	mergeMsg, err := svc.MergePullRequest(ctx, "davidnzube101", "shipp", 7)
	if err != nil {
		t.Fatalf("MergePullRequest failed: %v", err)
	}
	if !strings.Contains(mergeMsg, "successfully merged") {
		t.Errorf("unexpected merge message: %s", mergeMsg)
	}

	// 6. Test Workflow Runs
	runs, err := svc.GetWorkflowRuns(ctx, "davidnzube101", "shipp", "")
	if err != nil || len(runs) == 0 {
		t.Fatalf("GetWorkflowRuns failed: %v", err)
	}
	formattedRuns := FormatWorkflowRuns("davidnzube101", "shipp", runs)
	if !strings.Contains(formattedRuns, "CI") || !strings.Contains(formattedRuns, "success") {
		t.Errorf("unexpected formatted runs: %s", formattedRuns)
	}

	// 7. Test Releases
	releases, err := svc.GetReleases(ctx, "davidnzube101", "shipp", "")
	if err != nil || len(releases) == 0 {
		t.Fatalf("GetReleases failed: %v", err)
	}
	formattedReleases := FormatReleases("davidnzube101", "shipp", releases)
	if !strings.Contains(formattedReleases, "v1.0.0") {
		t.Errorf("unexpected formatted releases: %s", formattedReleases)
	}

	// 8. Test Commits
	commits, err := svc.GetCommits(ctx, "davidnzube101", "shipp", "", "")
	if err != nil || len(commits) == 0 {
		t.Fatalf("GetCommits failed: %v", err)
	}
	formattedCommits := FormatCommits("davidnzube101", "shipp", commits)
	if !strings.Contains(formattedCommits, "d640e04") {
		t.Errorf("unexpected formatted commits: %s", formattedCommits)
	}

	// 9. Test Issues
	issues, err := svc.GetIssues(ctx, "davidnzube101", "shipp", "")
	if err != nil || len(issues) == 0 {
		t.Fatalf("GetIssues failed: %v", err)
	}
	formattedIssues := FormatIssues("davidnzube101", "shipp", issues)
	if !strings.Contains(formattedIssues, "#3") || !strings.Contains(formattedIssues, "Support QR code") {
		t.Errorf("unexpected formatted issues: %s", formattedIssues)
	}
}
