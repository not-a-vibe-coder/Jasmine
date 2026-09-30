package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Service struct {
	token        string
	username     string
	defaultEmail string
	baseURL      string
	httpClient   *http.Client
}

type FileContent struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Size    int    `json:"size"`
	Type    string `json:"type"`
	Content string `json:"content"` // Base64 encoded
}

type CommitOptions struct {
	Branch      string
	Message     string
	Content     string
	FileSHA     string
	CustomPAT   string
	AuthorName  string
	AuthorEmail string
}

func NewService(token, username, defaultEmail string) *Service {
	return &Service{
		token:        token,
		username:     username,
		defaultEmail: defaultEmail,
		baseURL:      "https://api.github.com",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (s *Service) SetBaseURL(u string) {
	s.baseURL = u
}

// ParseRepoSlug parses "owner/repo" or "repo" (defaulting to config username) or full github.com URL
func (s *Service) ParseRepoSlug(input string) (string, string, error) {
	cleaned := strings.TrimSpace(input)
	cleaned = strings.TrimPrefix(cleaned, "https://github.com/")
	cleaned = strings.TrimPrefix(cleaned, "http://github.com/")
	cleaned = strings.TrimPrefix(cleaned, "github.com/")
	cleaned = strings.TrimSuffix(cleaned, ".git")
	cleaned = strings.Trim(cleaned, "/")

	parts := strings.Split(cleaned, "/")
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	if len(parts) == 1 && parts[0] != "" {
		if s.username != "" {
			return s.username, parts[0], nil
		}
		return "", "", fmt.Errorf("repository must be in 'owner/repo' format (e.g. davidnzube101/shipp)")
	}
	return "", "", fmt.Errorf("invalid repository format '%s'. Please specify 'owner/repo'", input)
}

func (s *Service) makeRequestWithToken(ctx context.Context, method, url string, body interface{}, customToken string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	tokenToUse := s.token
	if customToken != "" {
		tokenToUse = customToken
	}
	if tokenToUse != "" {
		req.Header.Set("Authorization", "Bearer "+tokenToUse)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return s.httpClient.Do(req)
}

func (s *Service) makeRequest(ctx context.Context, method, url string, body interface{}) (*http.Response, error) {
	return s.makeRequestWithToken(ctx, method, url, body, "")
}

// GetDefaultBranch retrieves the default branch (e.g. main, master) for a repository
func (s *Service) GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	return s.GetDefaultBranchWithToken(ctx, owner, repo, "")
}

func (s *Service) GetDefaultBranchWithToken(ctx context.Context, owner, repo, customToken string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("repository '%s/%s' not found or inaccessible", owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github api error (%d): %s", resp.StatusCode, string(respBytes))
	}

	var data struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.DefaultBranch == "" {
		return "main", nil
	}
	return data.DefaultBranch, nil
}

// GetFile retrieves a file's content and SHA from a repository
func (s *Service) GetFile(ctx context.Context, owner, repo, path, ref string) (*FileContent, string, error) {
	return s.GetFileWithToken(ctx, owner, repo, path, ref, "")
}

func (s *Service) GetFileWithToken(ctx context.Context, owner, repo, path, ref, customToken string) (*FileContent, string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))
	if ref != "" {
		url += "?ref=" + ref
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("file '%s' not found in '%s/%s'", path, owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("github api error (%d): %s", resp.StatusCode, string(respBytes))
	}

	var fc FileContent
	if err := json.NewDecoder(resp.Body).Decode(&fc); err != nil {
		return nil, "", err
	}

	// Base64 decode content
	rawContent := strings.ReplaceAll(fc.Content, "\n", "")
	decodedBytes, err := base64.StdEncoding.DecodeString(rawContent)
	if err != nil {
		return &fc, "", fmt.Errorf("failed to decode base64 file content: %w", err)
	}

	return &fc, string(decodedBytes), nil
}

// ListDirectory lists file names in a repository path
func (s *Service) ListDirectory(ctx context.Context, owner, repo, path, ref string) ([]string, error) {
	return s.ListDirectoryWithToken(ctx, owner, repo, path, ref, "")
}

func (s *Service) ListDirectoryWithToken(ctx context.Context, owner, repo, path, ref, customToken string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))
	if ref != "" {
		url += "?ref=" + ref
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list directory: status %d", resp.StatusCode)
	}

	var items []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var names []string
	for _, it := range items {
		if it.Type == "dir" {
			names = append(names, it.Name+"/")
		} else {
			names = append(names, it.Name)
		}
	}
	return names, nil
}

// GetBranchRef gets the latest commit SHA of a branch
func (s *Service) GetBranchRef(ctx context.Context, owner, repo, branch string) (string, error) {
	return s.GetBranchRefWithToken(ctx, owner, repo, branch, "")
}

func (s *Service) GetBranchRefWithToken(ctx context.Context, owner, repo, branch, customToken string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", s.baseURL, owner, repo, branch)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get branch ref for %s: %s", branch, string(respBytes))
	}

	var refData struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&refData); err != nil {
		return "", err
	}
	return refData.Object.SHA, nil
}

// CreateBranch creates a new branch pointing to the baseBranch commit SHA
func (s *Service) CreateBranch(ctx context.Context, owner, repo, newBranch, baseBranch string) error {
	return s.CreateBranchWithToken(ctx, owner, repo, newBranch, baseBranch, "")
}

func (s *Service) CreateBranchWithToken(ctx context.Context, owner, repo, newBranch, baseBranch, customToken string) error {
	baseSHA, err := s.GetBranchRefWithToken(ctx, owner, repo, baseBranch, customToken)
	if err != nil {
		return fmt.Errorf("failed to resolve base branch '%s': %w", baseBranch, err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/git/refs", s.baseURL, owner, repo)
	payload := map[string]string{
		"ref": "refs/heads/" + newBranch,
		"sha": baseSHA,
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPost, url, payload, customToken)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create branch '%s' (%d): %s", newBranch, resp.StatusCode, string(respBytes))
	}

	return nil
}

// IsRepoEmpty checks if a repository has no commits / no branches yet.
func (s *Service) IsRepoEmpty(ctx context.Context, owner, repo string) (bool, error) {
	return s.IsRepoEmptyWithToken(ctx, owner, repo, "")
}

// IsRepoEmptyWithToken checks if a repository has no commits / no branches yet.
func (s *Service) IsRepoEmptyWithToken(ctx context.Context, owner, repo, customToken string) (bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/branches", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var branches []struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&branches); err == nil {
			return len(branches) == 0, nil
		}
	} else if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound {
		return true, nil
	}
	return false, nil
}

// CommitFile commits a file update or creation with optional custom author/committer credentials
func (s *Service) CommitFileWithOptions(ctx context.Context, owner, repo, path string, opts CommitOptions) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))

	authorName := strings.TrimSpace(opts.AuthorName)
	if authorName == "" {
		authorName = "Shipp" // Strictly "Shipp" by default
	}

	authorEmail := strings.TrimSpace(opts.AuthorEmail)
	if authorEmail == "" {
		authorEmail = s.defaultEmail
	}
	if authorEmail == "" {
		authorEmail = "shippzero@atomicmail.io"
	}

	payload := map[string]interface{}{
		"message": opts.Message,
		"content": base64.StdEncoding.EncodeToString([]byte(opts.Content)),
		"branch":  opts.Branch,
		"committer": map[string]string{
			"name":  authorName,
			"email": authorEmail,
		},
		"author": map[string]string{
			"name":  authorName,
			"email": authorEmail,
		},
	}
	if opts.FileSHA != "" {
		payload["sha"] = opts.FileSHA
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPut, url, payload, opts.CustomPAT)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("commit failed (%d): %s", resp.StatusCode, string(respBytes))
	}

	var commitResp struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Content struct {
			HTMLURL string `json:"html_url"`
		} `json:"content"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&commitResp)

	return commitResp.Content.HTMLURL, nil
}

// CommitFile is backward-compatible wrapper calling CommitFileWithOptions with defaults
func (s *Service) CommitFile(ctx context.Context, owner, repo, path, branch, message, content, fileSHA string) (string, error) {
	return s.CommitFileWithOptions(ctx, owner, repo, path, CommitOptions{
		Branch:  branch,
		Message: message,
		Content: content,
		FileSHA: fileSHA,
	})
}

// CreatePullRequest opens a Pull Request on GitHub
func (s *Service) CreatePullRequest(ctx context.Context, owner, repo, title, body, headBranch, baseBranch string) (string, int, error) {
	return s.CreatePullRequestWithToken(ctx, owner, repo, title, body, headBranch, baseBranch, "")
}

func (s *Service) CreatePullRequestWithToken(ctx context.Context, owner, repo, title, body, headBranch, baseBranch, customToken string) (string, int, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", s.baseURL, owner, repo)

	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  headBranch,
		"base":  baseBranch,
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPost, url, payload, customToken)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("failed to create pull request (%d): %s", resp.StatusCode, string(respBytes))
	}

	var prResp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prResp); err != nil {
		return "", 0, err
	}

	return prResp.HTMLURL, prResp.Number, nil
}

// MergePullRequest merges an open Pull Request on GitHub
func (s *Service) MergePullRequest(ctx context.Context, owner, repo string, pullNumber int) (string, error) {
	return s.MergePullRequestWithToken(ctx, owner, repo, pullNumber, "")
}

func (s *Service) MergePullRequestWithToken(ctx context.Context, owner, repo string, pullNumber int, customToken string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", s.baseURL, owner, repo, pullNumber)

	payload := map[string]string{
		"commit_title": fmt.Sprintf("Merge PR #%d via Shipp Bot", pullNumber),
		"merge_method": "squash",
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPut, url, payload, customToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to merge PR #%d (%d): %s", pullNumber, resp.StatusCode, string(respBytes))
	}

	var mergeResp struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&mergeResp)

	if !mergeResp.Merged {
		return "", fmt.Errorf("pull request #%d could not be merged: %s", pullNumber, mergeResp.Message)
	}

	return fmt.Sprintf("PR #%d successfully merged (SHA: %s)", pullNumber, mergeResp.SHA[:7]), nil
}

// ClosePullRequest closes an open Pull Request on GitHub without merging it.
func (s *Service) ClosePullRequest(ctx context.Context, owner, repo string, pullNumber int) (string, error) {
	return s.ClosePullRequestWithToken(ctx, owner, repo, pullNumber, "")
}

func (s *Service) ClosePullRequestWithToken(ctx context.Context, owner, repo string, pullNumber int, customToken string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", s.baseURL, owner, repo, pullNumber)

	payload := map[string]string{
		"state": "closed",
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPatch, url, payload, customToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to close PR #%d (%d): %s", pullNumber, resp.StatusCode, string(respBytes))
	}

	return fmt.Sprintf("PR #%d on %s/%s successfully closed", pullNumber, owner, repo), nil
}

// CloseIssue closes an open Issue on GitHub.
func (s *Service) CloseIssue(ctx context.Context, owner, repo string, issueNumber int, reason string) (string, error) {
	return s.CloseIssueWithToken(ctx, owner, repo, issueNumber, reason, "")
}

func (s *Service) CloseIssueWithToken(ctx context.Context, owner, repo string, issueNumber int, reason string, customToken string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", s.baseURL, owner, repo, issueNumber)

	payload := map[string]string{
		"state": "closed",
	}
	if reason == "not_planned" || reason == "completed" {
		payload["state_reason"] = reason
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPatch, url, payload, customToken)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to close issue #%d (%d): %s", issueNumber, resp.StatusCode, string(respBytes))
	}

	return fmt.Sprintf("Issue #%d on %s/%s successfully closed", issueNumber, owner, repo), nil
}

// -------------------------------------------------------------
// Expanded GitHub Project Intelligence (Actions, Releases, Commits, Issues)
// -------------------------------------------------------------

type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Status     string    `json:"status"`     // queued, in_progress, completed
	Conclusion string    `json:"conclusion"` // success, failure, neutral, cancelled, timed_out
	HTMLURL    string    `json:"html_url"`
	Event      string    `json:"event"`
	CreatedAt  time.Time `json:"created_at"`
	Actor      struct {
		Login string `json:"login"`
	} `json:"actor"`
	HeadCommit struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

func (s *Service) GetWorkflowRuns(ctx context.Context, owner, repo, customToken string) ([]WorkflowRun, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?per_page=5", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get workflow runs (%d): %s", resp.StatusCode, string(respBytes))
	}

	var data struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	return data.WorkflowRuns, nil
}

type Release struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Assets []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (s *Service) GetReleases(ctx context.Context, owner, repo, customToken string) ([]Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=5", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get releases (%d): %s", resp.StatusCode, string(respBytes))
	}

	var data []Release
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	return data, nil
}

type CommitInfo struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (s *Service) GetCommits(ctx context.Context, owner, repo, branch, customToken string) ([]CommitInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits?per_page=5", s.baseURL, owner, repo)
	if branch != "" {
		url += "&sha=" + branch
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get commits (%d): %s", resp.StatusCode, string(respBytes))
	}

	var data []CommitInfo
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	return data, nil
}

type IssueInfo struct {
	Number      int         `json:"number"`
	Title       string      `json:"title"`
	State       string      `json:"state"`
	HTMLURL     string      `json:"html_url"`
	CreatedAt   time.Time   `json:"created_at"`
	PullRequest interface{} `json:"pull_request,omitempty"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (s *Service) GetIssues(ctx context.Context, owner, repo, customToken string) ([]IssueInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues?state=all&per_page=10", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get issues (%d): %s", resp.StatusCode, string(respBytes))
	}

	var raw []IssueInfo
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	// Filter out PRs
	var issues []IssueInfo
	for _, it := range raw {
		if it.PullRequest == nil {
			issues = append(issues, it)
			if len(issues) >= 5 {
				break
			}
		}
	}

	return issues, nil
}

type RepoOverview struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Language      string `json:"language"`
	Stargazers    int    `json:"stargazers_count"`
	Forks         int    `json:"forks_count"`
	OpenIssues    int    `json:"open_issues_count"`
	Visibility    string `json:"visibility"`
}

func (s *Service) GetRepoOverview(ctx context.Context, owner, repo, customToken string) (*RepoOverview, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", s.baseURL, owner, repo)
	resp, err := s.makeRequestWithToken(ctx, http.MethodGet, url, nil, customToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get repo overview (%d): %s", resp.StatusCode, string(respBytes))
	}

	var overview RepoOverview
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		return nil, err
	}

	return &overview, nil
}

// -------------------------------------------------------------
// Formatters for Project Intelligence
// -------------------------------------------------------------

func FormatWorkflowRuns(owner, repo string, runs []WorkflowRun) string {
	if len(runs) == 0 {
		return fmt.Sprintf("No recent GitHub Actions workflow runs found on %s/%s.", owner, repo)
	}

	latest := runs[0]
	name := latest.Name
	if name == "" {
		name = "Workflow"
	}

	statusDesc := latest.Status
	if latest.Conclusion != "" {
		statusDesc = latest.Conclusion
	}

	return fmt.Sprintf("The latest '%s' run on %s/%s is %s on %s: %s",
		name, owner, repo, statusDesc, latest.HeadBranch, latest.HTMLURL)
}

func FormatReleases(owner, repo string, releases []Release) string {
	if len(releases) == 0 {
		return fmt.Sprintf("No releases published yet on %s/%s.", owner, repo)
	}

	latest := releases[0]
	tag := latest.TagName
	if tag == "" {
		tag = latest.Name
	}
	dateStr := ""
	if latest.PublishedAt != nil {
		dateStr = " (" + latest.PublishedAt.Format("Jan 02, 2006") + ")"
	}

	return fmt.Sprintf("Latest release on %s/%s is %s%s: %s", owner, repo, tag, dateStr, latest.HTMLURL)
}

func FormatCommits(owner, repo string, commits []CommitInfo) string {
	if len(commits) == 0 {
		return fmt.Sprintf("No commits found on %s/%s.", owner, repo)
	}

	latest := commits[0]
	shortSHA := latest.SHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	firstLine := strings.Split(latest.Commit.Message, "\n")[0]
	author := latest.Commit.Author.Name
	if latest.Author.Login != "" {
		author = "@" + latest.Author.Login
	}

	return fmt.Sprintf("Latest commit on %s/%s is '%s' (%s) by %s: %s",
		owner, repo, firstLine, shortSHA, author, latest.HTMLURL)
}

func FormatIssues(owner, repo string, issues []IssueInfo) string {
	if len(issues) == 0 {
		return fmt.Sprintf("No open issues found on %s/%s.", owner, repo)
	}

	latest := issues[0]
	return fmt.Sprintf("%s/%s has %d issue(s). Latest is #%d: '%s' by @%s (%s)",
		owner, repo, len(issues), latest.Number, latest.Title, latest.User.Login, latest.HTMLURL)
}

func FormatRepoOverview(overview *RepoOverview) string {
	if overview == nil {
		return "No repository details available."
	}

	desc := overview.Description
	if desc == "" {
		desc = "no description"
	}

	return fmt.Sprintf("%s (%s, branch: %s): %s | %d stars, %d forks. %s",
		overview.FullName, overview.Language, overview.DefaultBranch, desc, overview.Stargazers, overview.Forks, overview.HTMLURL)
}

type CreateRepoOptions struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private"`
	AutoInit    bool   `json:"auto_init"`
	Org         string `json:"-"`
	CustomPAT   string `json:"-"`
}

// CreateRepository creates a new repository on GitHub under the authenticated user (or specified organization).
func (s *Service) CreateRepository(ctx context.Context, opts CreateRepoOptions) (string, error) {
	url := fmt.Sprintf("%s/user/repos", s.baseURL)
	if opts.Org != "" {
		url = fmt.Sprintf("%s/orgs/%s/repos", s.baseURL, opts.Org)
	}

	payload := map[string]interface{}{
		"name":      opts.Name,
		"private":   opts.Private,
		"auto_init": opts.AutoInit,
	}
	if opts.Description != "" {
		payload["description"] = opts.Description
	}

	resp, err := s.makeRequestWithToken(ctx, http.MethodPost, url, payload, opts.CustomPAT)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github api returned status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return res.HTMLURL, nil
}

