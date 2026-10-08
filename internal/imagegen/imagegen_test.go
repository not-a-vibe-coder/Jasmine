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
