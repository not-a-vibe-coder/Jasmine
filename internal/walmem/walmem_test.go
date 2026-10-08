package walmem

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"
)

const testSeed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config" {
			_, _ = w.Write([]byte(`{"packageId":"0xpkg","network":"mainnet"}`))
			return
		}
		if (r.URL.Path == "/api/recall") != (r.Header.Get("x-seal-session") != "") {
			http.Error(w, "seal session must be sent on decrypting routes only", http.StatusBadRequest)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "0xabc", testSeed)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Unix(1700000000, 0) }
	return c
}

func TestNewClientOptional(t *testing.T) {
	c, err := NewClient("", "", "")
	if c != nil || err != nil {
		t.Fatalf("expected nil client and nil error when unconfigured, got %v %v", c, err)
	}
	if _, err := NewClient("", "0xabc", "zz"); err == nil {
		t.Fatal("expected error for bad hex")
	}
	if _, err := NewClient("", "0xabc", "abcd"); err == nil {
		t.Fatal("expected error for wrong key length")
	}
}

func TestSignedRequestVerifies(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		pub, _ := hex.DecodeString(r.Header.Get("x-public-key"))
		sig, _ := hex.DecodeString(r.Header.Get("x-signature"))
		sum := sha256.Sum256(body)
		msg := strings.Join([]string{
			r.Header.Get("x-timestamp"), r.Method, r.URL.RequestURI(),
			hex.EncodeToString(sum[:]), r.Header.Get("x-nonce"), r.Header.Get("x-account-id"),
		}, ".")
		if !ed25519.Verify(pub, []byte(msg), sig) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-timestamp") != "1700000000" || r.Header.Get("x-account-id") != "0xabc" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		if len(r.Header.Get("x-nonce")) != 36 {
			http.Error(w, "bad nonce", http.StatusBadRequest)
			return
		}
		var req map[string]interface{}
		_ = json.Unmarshal(body, &req)
		if req["namespace"] != "tg-user-42" || req["query"] != "what do i like" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"blob_id":"b1","text":"likes jollof","distance":0.2},{"blob_id":"b2","text":"far away","distance":0.95}],"total":2}`))
	})

	mems, err := c.Recall(t.Context(), UserNamespace(42), "what do i like", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 || mems[0].BlobID != "b1" {
		t.Fatalf("unexpected memories: %+v", mems)
	}
	if got := FormatForPrompt(mems, 0.8); got != "- likes jollof" {
		t.Fatalf("FormatForPrompt filtered wrong: %q", got)
	}
}

func TestGetSignsEmptyBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pub, _ := hex.DecodeString(r.Header.Get("x-public-key"))
		sig, _ := hex.DecodeString(r.Header.Get("x-signature"))
		msg := signingMessage(r.Header.Get("x-timestamp"), "GET", "/api/remember/job-1", nil, r.Header.Get("x-nonce"), "0xabc")
		if !ed25519.Verify(pub, []byte(msg), sig) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"job_id":"job-1","status":"done","blob_id":"blobX"}`))
	})
	st, err := c.WaitForBlob(t.Context(), "job-1")
	if err != nil || st.BlobID != "blobX" {
		t.Fatalf("got %+v %v", st, err)
	}
}

func TestErrorStatusSurfaces(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	if _, err := c.Stats(t.Context(), "ns"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}

func TestNamespacesAndLinks(t *testing.T) {
	if UserNamespace(7) != "tg-user-7" || GroupNamespace(-1001) != "tg-group--1001" {
		t.Fatal("namespace format changed")
	}
	if BlobURL("abc") != "https://walruscan.com/mainnet/blob/abc" {
		t.Fatal("blob url format changed")
	}
}

func TestSealSessionShape(t *testing.T) {
	seed, _ := hex.DecodeString(testSeed)
	priv := ed25519.NewKeyFromSeed(seed)
	now := time.Date(2026, 10, 8, 1, 2, 3, 0, time.UTC)
	hdr, err := buildSealSession(priv, "0xpkg", now, strings.NewReader(strings.Repeat("k", 64)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Address   string `json:"address"`
		PackageID string `json:"packageId"`
		Creation  int64  `json:"creationTimeMs"`
		TTL       int    `json:"ttlMin"`
		Sig       string `json:"personalMessageSignature"`
		Session   string `json:"sessionKey"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PackageID != "0xpkg" || got.TTL != 5 || got.Creation != now.UnixMilli() || !strings.HasPrefix(got.Session, "suiprivkey1") {
		t.Fatalf("unexpected session: %+v", got)
	}
	if got.Address != suiAddress(priv.Public().(ed25519.PublicKey)) || len(got.Address) != 66 {
		t.Fatalf("bad address %s", got.Address)
	}

	// Signature verifies over the Sui PersonalMessage intent digest.
	sessionSeed, err := decodeSuiPrivKey(got.Session)
	if err != nil {
		t.Fatal(err)
	}
	sessionPub := ed25519.NewKeyFromSeed(sessionSeed).Public().(ed25519.PublicKey)
	msg := "Accessing keys of package 0xpkg for 5 mins from 2026-10-08 01:02:03 UTC, session key " + base64.StdEncoding.EncodeToString(sessionPub)
	sigBytes, _ := base64.StdEncoding.DecodeString(got.Sig)
	if len(sigBytes) != 97 || sigBytes[0] != 0 {
		t.Fatalf("bad serialized signature length %d", len(sigBytes))
	}
	h, _ := blake2b.New256(nil)
	h.Write([]byte{3, 0, 0})
	h.Write(append(uleb128(uint64(len(msg))), msg...))
	if !ed25519.Verify(sigBytes[65:], h.Sum(nil), sigBytes[1:65]) {
		t.Fatal("personal message signature does not verify")
	}
}

func TestBech32RoundTrip(t *testing.T) {
	seed, _ := hex.DecodeString(testSeed)
	enc, err := suiPrivKeyBech32(seed)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeSuiPrivKey(enc)
	if err != nil || hex.EncodeToString(dec) != testSeed {
		t.Fatalf("round trip failed: %v %x", err, dec)
	}
	c, err := NewClient("", "0xabc", enc)
	if err != nil || c == nil {
		t.Fatalf("suiprivkey input rejected: %v", err)
	}
}

func TestULEB128(t *testing.T) {
	if hex.EncodeToString(uleb128(300)) != "ac02" || hex.EncodeToString(uleb128(5)) != "05" {
		t.Fatal("uleb128 encoding wrong")
	}
}
