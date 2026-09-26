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
	token      string
	username   string
	baseURL    string
	httpClient *http.Client
}

type FileContent struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Size    int    `json:"size"`
	Type    string `json:"type"`
	Content string `json:"content"` // Base64 encoded
}

func NewService(token, username string) *Service {
	return &Service{
		token:    token,
		username: username,
		baseURL:  "https://api.github.com",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
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

func (s *Service) makeRequest(ctx context.Context, method, url string, body interface{}) (*http.Response, error) {
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
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return s.httpClient.Do(req)
}

// GetDefaultBranch retrieves the default branch (e.g. main, master) for a repository
func (s *Service) GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", s.baseURL, owner, repo)
	resp, err := s.makeRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("repository '%s/%s' not found or inaccessible (check permissions for @%s)", owner, repo, s.username)
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
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))
	if ref != "" {
		url += "?ref=" + ref
	}

	resp, err := s.makeRequest(ctx, http.MethodGet, url, nil)
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
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))
	if ref != "" {
		url += "?ref=" + ref
	}

	resp, err := s.makeRequest(ctx, http.MethodGet, url, nil)
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
	url := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s", s.baseURL, owner, repo, branch)
	resp, err := s.makeRequest(ctx, http.MethodGet, url, nil)
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
	baseSHA, err := s.GetBranchRef(ctx, owner, repo, baseBranch)
	if err != nil {
		return fmt.Errorf("failed to resolve base branch '%s': %w", baseBranch, err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/git/refs", s.baseURL, owner, repo)
	payload := map[string]string{
		"ref": "refs/heads/" + newBranch,
		"sha": baseSHA,
	}

	resp, err := s.makeRequest(ctx, http.MethodPost, url, payload)
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

// CommitFile commits a file update or creation to the specified branch
func (s *Service) CommitFile(ctx context.Context, owner, repo, path, branch, message, content, fileSHA string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.baseURL, owner, repo, strings.TrimPrefix(path, "/"))

	payload := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if fileSHA != "" {
		payload["sha"] = fileSHA
	}

	resp, err := s.makeRequest(ctx, http.MethodPut, url, payload)
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

// CreatePullRequest opens a Pull Request on GitHub
func (s *Service) CreatePullRequest(ctx context.Context, owner, repo, title, body, headBranch, baseBranch string) (string, int, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", s.baseURL, owner, repo)

	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  headBranch,
		"base":  baseBranch,
	}

	resp, err := s.makeRequest(ctx, http.MethodPost, url, payload)
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
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", s.baseURL, owner, repo, pullNumber)

	payload := map[string]string{
		"commit_title":   fmt.Sprintf("Merge PR #%d via Shipp Bot", pullNumber),
		"merge_method":   "squash",
	}

	resp, err := s.makeRequest(ctx, http.MethodPut, url, payload)
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
