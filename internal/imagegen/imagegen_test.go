package imagegen

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPollinationsFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "walrus in shades") {
			t.Errorf("prompt not in path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("nologo") != "true" {
			t.Errorf("nologo not set")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff})
	}))
	defer srv.Close()

	s := NewService("", "", "")
	s.SetBaseURLs(srv.URL+"/prompt/", "")
	img, err := s.Generate(t.Context(), "walrus in shades")
	if err != nil {
		t.Fatal(err)
	}
	// No key: the keyed models are skipped and the legacy keyless endpoint is used.
	if img.Provider != "pollinations" || img.MIMEType != "image/jpeg" || len(img.Data) != 3 {
		t.Fatalf("unexpected image: %+v", img)
	}
}

func TestFallsBackToGemini(t *testing.T) {
	poll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	defer poll.Close()
	png := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	gem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gkey" || !strings.HasSuffix(r.URL.Path, "/test-model:generateContent") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"here"},{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]}}]}`))
	}))
	defer gem.Close()

	s := NewService("", "gkey", "test-model")
	s.SetBaseURLs(poll.URL+"/prompt/", gem.URL+"/models/")
	img, err := s.Generate(t.Context(), "a cat")
	if err != nil {
		t.Fatal(err)
	}
	if img.Provider != "gemini" || string(img.Data) != "PNGDATA" {
		t.Fatalf("unexpected image: %+v", img)
	}
}

func TestNoFallbackWithoutKey(t *testing.T) {
	poll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>error</html>"))
	}))
	defer poll.Close()
	s := NewService("", "", "")
	s.SetBaseURLs(poll.URL+"/prompt/", "")
	if _, err := s.Generate(t.Context(), "x"); err == nil {
		t.Fatal("expected error when pollinations returns non-image and no gemini key")
	}
	if _, err := s.Generate(t.Context(), "  "); err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestModelChainAndKey(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		seen = append(seen, model+"|"+r.Header.Get("Authorization"))
		if model == "first" {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("IMG"))
	}))
	defer srv.Close()

	s := NewService("pk_test", "", "", "first", "second")
	s.SetBaseURLs(srv.URL+"/image/", "")
	img, err := s.Generate(t.Context(), "a fox")
	if err != nil {
		t.Fatal(err)
	}
	if img.Provider != "pollinations/second" {
		t.Fatalf("expected fallback to second model, got %s", img.Provider)
	}
	if len(seen) != 2 || seen[0] != "first|Bearer pk_test" || seen[1] != "second|Bearer pk_test" {
		t.Fatalf("unexpected requests: %v", seen)
	}
}

func TestHuggingFaceBeforeLegacy(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nrest"))
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hf_x" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + png + `"}]}`))
	}))
	defer hf.Close()
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("legacy endpoint should not be called when Hugging Face works")
	}))
	defer legacy.Close()

	s := NewService("", "", "")
	s.SetBaseURLs(legacy.URL+"/prompt/", "")
	s.hfURL = hf.URL
	s.SetHuggingFace("hf_x", "")
	img, err := s.Generate(t.Context(), "a walrus")
	if err != nil {
		t.Fatal(err)
	}
	if img.Provider != "huggingface/"+DefaultHFModel || img.MIMEType != "image/png" {
		t.Fatalf("unexpected image: %+v", img)
	}
}
