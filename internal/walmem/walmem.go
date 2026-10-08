// Package walmem is a native Go client for the Walrus Memory (MemWal) relayer.
//
// Memories are embedded, Seal-encrypted and stored as blobs on Walrus by the relayer;
// this client only signs requests with the account's Ed25519 delegate key.
// Request signing follows the relayer spec:
//
//	{timestamp}.{method}.{path_and_query}.{sha256(body)}.{nonce}.{account_id}
package walmem

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DefaultServerURL = "https://relayer.memory.walrus.xyz"

type Client struct {
	serverURL  string
	accountID  string
	privKey    ed25519.PrivateKey
	pubHex     string
	httpClient *http.Client
	now        func() time.Time
	seal       sealSessionCache
}

// Memory is a single recalled memory.
type Memory struct {
	BlobID   string  `json:"blob_id"`
	Text     string  `json:"text"`
	Distance float64 `json:"distance"`
}

type Stats struct {
	MemoryCount  int   `json:"memory_count"`
	StorageBytes int64 `json:"storage_bytes"`
}

type JobStatus struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	BlobID string `json:"blob_id"`
	Error  string `json:"error,omitempty"`
}

type AnalyzeResult struct {
	JobIDs    []string        `json:"job_ids"`
	Facts     json.RawMessage `json:"facts"`
	FactCount int             `json:"fact_count"`
}

type Whoami struct {
	AccountID string `json:"account_id"`
	Owner     string `json:"owner"`
	PackageID string `json:"package_id"`
}

// NewClient builds a client from a hex-encoded delegate key. The key may be the 32-byte
// Ed25519 seed or the 64-byte expanded private key, with or without a 0x prefix.
// Returns nil (not an error) when accountID or key is empty so callers can treat
// Walrus Memory as optional.
func NewClient(serverURL, accountID, delegateKeyHex string) (*Client, error) {
	delegateKeyHex = strings.TrimPrefix(strings.TrimSpace(delegateKeyHex), "0x")
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || delegateKeyHex == "" {
		return nil, nil
	}
	if serverURL == "" {
		serverURL = DefaultServerURL
	}
	var raw []byte
	var err error
	if strings.HasPrefix(strings.ToLower(delegateKeyHex), "suiprivkey1") {
		raw, err = decodeSuiPrivKey(delegateKeyHex)
	} else {
		raw, err = hex.DecodeString(delegateKeyHex)
	}
	if err != nil {
		return nil, fmt.Errorf("delegate key must be hex or suiprivkey1...: %w", err)
	}
	var priv ed25519.PrivateKey
	switch len(raw) {
	case ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(raw)
	case ed25519.PrivateKeySize:
		priv = ed25519.PrivateKey(raw)
	default:
		return nil, fmt.Errorf("delegate key must be 32 or 64 bytes, got %d", len(raw))
	}
	return &Client{
		serverURL:  strings.TrimRight(serverURL, "/"),
		accountID:  accountID,
		privKey:    priv,
		pubHex:     hex.EncodeToString(priv.Public().(ed25519.PublicKey)),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
	}, nil
}

func (c *Client) AccountID() string { return c.accountID }

// SetHTTPClient is used by tests to point at an httptest server.
func (c *Client) SetHTTPClient(h *http.Client) { c.httpClient = h }

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// signingMessage builds the exact string the relayer verifies.
func signingMessage(timestamp, method, pathAndQuery string, body []byte, nonce, accountID string) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{timestamp, method, pathAndQuery, hex.EncodeToString(sum[:]), nonce, accountID}, ".")
}

// Routes that decrypt memories server-side need a Seal session credential.
var sealRoutes = map[string]bool{"/api/recall": true, "/api/ask": true, "/api/restore": true}

func (c *Client) do(ctx context.Context, method, path string, in, out interface{}) error {
	var sealHeader string
	if sealRoutes[path] {
		var err error
		if sealHeader, err = c.sealSession(ctx); err != nil {
			return fmt.Errorf("walrus memory seal session: %w", err)
		}
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	ts := strconv.FormatInt(c.now().Unix(), 10)
	nonce := newNonce()
	sig := ed25519.Sign(c.privKey, []byte(signingMessage(ts, method, path, body, nonce, c.accountID)))

	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-public-key", c.pubHex)
	req.Header.Set("x-signature", hex.EncodeToString(sig))
	req.Header.Set("x-timestamp", ts)
	req.Header.Set("x-nonce", nonce)
	req.Header.Set("x-account-id", c.accountID)
	if sealHeader != "" {
		req.Header.Set("x-seal-session", sealHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("walrus memory %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := strings.TrimSpace(string(respBody))
		if code := resp.Header.Get("x-auth-error"); code != "" {
			reason = code + " " + reason
		}
		if resp.StatusCode == http.StatusUnauthorized && reason == "" {
			reason = "delegate key not registered on this account (check MEMWAL_DELEGATE_KEY is the PRIVATE key)"
		}
		return fmt.Errorf("walrus memory %s %s: status %d: %s", method, path, resp.StatusCode, reason)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("walrus memory %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	var out Whoami
	if err := c.do(ctx, http.MethodGet, "/api/whoami", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Recall runs a semantic search over a namespace.
func (c *Client) Recall(ctx context.Context, namespace, query string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 5
	}
	var out struct {
		Results []Memory `json:"results"`
	}
	err := c.do(ctx, http.MethodPost, "/api/recall", map[string]interface{}{
		"query":     query,
		"limit":     limit,
		"namespace": namespace,
	}, &out)
	return out.Results, err
}

// Remember stores one memory verbatim and returns the async job id.
func (c *Client) Remember(ctx context.Context, namespace, text string) (string, error) {
	var out JobStatus
	err := c.do(ctx, http.MethodPost, "/api/remember", map[string]string{"text": text, "namespace": namespace}, &out)
	return out.JobID, err
}

func (c *Client) RememberStatus(ctx context.Context, jobID string) (*JobStatus, error) {
	var out JobStatus
	if err := c.do(ctx, http.MethodGet, "/api/remember/"+jobID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitForBlob polls a remember job until it has a blob id, fails, or ctx expires.
func (c *Client) WaitForBlob(ctx context.Context, jobID string) (*JobStatus, error) {
	for {
		st, err := c.RememberStatus(ctx, jobID)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(st.Status) {
		case "done", "completed", "success":
			return st, nil
		case "failed", "error":
			return st, fmt.Errorf("remember job %s failed: %s", jobID, st.Error)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Analyze lets the relayer extract durable facts from free text and store each one.
func (c *Client) Analyze(ctx context.Context, namespace, text string) (*AnalyzeResult, error) {
	var out AnalyzeResult
	err := c.do(ctx, http.MethodPost, "/api/analyze", map[string]string{
		"text":        text,
		"namespace":   namespace,
		"occurred_at": c.now().UTC().Format(time.RFC3339),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Stats(ctx context.Context, namespace string) (*Stats, error) {
	var out Stats
	if err := c.do(ctx, http.MethodPost, "/api/stats", map[string]string{"namespace": namespace}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Forget deletes every memory in a namespace and returns how many were removed.
func (c *Client) Forget(ctx context.Context, namespace string) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	err := c.do(ctx, http.MethodPost, "/api/forget", map[string]string{"namespace": namespace}, &out)
	return out.Deleted, err
}

// Restore rebuilds the relayer's search index for a namespace from Walrus blobs.
func (c *Client) Restore(ctx context.Context, namespace string, limit int) (int, error) {
	var out struct {
		Restored int `json:"restored"`
	}
	err := c.do(ctx, http.MethodPost, "/api/restore", map[string]interface{}{"namespace": namespace, "limit": limit}, &out)
	return out.Restored, err
}

// UserNamespace scopes memories to one Telegram user across every chat and device.
func UserNamespace(userID int64) string { return "tg-user-" + strconv.FormatInt(userID, 10) }

// GroupNamespace scopes shared memories to one Telegram group.
func GroupNamespace(chatID int64) string { return "tg-group-" + strconv.FormatInt(chatID, 10) }

// BlobURL links a blob to the Walrus explorer.
func BlobURL(blobID string) string { return "https://walruscan.com/mainnet/blob/" + blobID }

// FormatForPrompt renders recalled memories as a compact prompt block.
// Memories farther than maxDistance (cosine) are dropped as irrelevant.
func FormatForPrompt(mems []Memory, maxDistance float64) string {
	var lines []string
	seen := map[string]bool{}
	for _, m := range mems {
		t := strings.TrimSpace(m.Text)
		if t == "" || seen[t] || (maxDistance > 0 && m.Distance > maxDistance) {
			continue
		}
		seen[t] = true
		lines = append(lines, "- "+t)
	}
	return strings.Join(lines, "\n")
}
