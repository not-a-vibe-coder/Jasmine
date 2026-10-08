package voice

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanForSpeech(t *testing.T) {
	in := "**hey jack** 😂 <laugh> check https://walruscan.com/x and `code`"
	if got := CleanForSpeech(in, false); got != "hey jack check and code" {
		t.Errorf("edge clean = %q", got)
	}
	if got := CleanForSpeech(in, true); got != "hey jack <laugh> check and code" {
		t.Errorf("orpheus clean = %q", got)
	}
	if got := CleanForSpeech(strings.Repeat("a", 2000), false); len(got) != maxSpeechChars {
		t.Errorf("long text not capped: %d", len(got))
	}
}

func TestTranscribeGroq(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer gk" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("model") != whisperModel {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f, _, _ := r.FormFile("file")
		data, _ := io.ReadAll(f)
		if string(data) != "OGGDATA" {
			http.Error(w, "bad file", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"text":" remind me about friday "}`))
	}))
	defer srv.Close()

	s := NewService("gk", "", "", "")
	s.groqBaseURL = srv.URL
	got, err := s.Transcribe(t.Context(), []byte("OGGDATA"), "voice.ogg", "audio/ogg")
	if err != nil || got != "remind me about friday" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTranscribeNoProvider(t *testing.T) {
	if _, err := NewService("", "", "", "").Transcribe(t.Context(), []byte("x"), "", ""); err == nil {
		t.Fatal("expected an error with no providers")
	}
}

func TestOrpheusOffWithoutKey(t *testing.T) {
	if !NewService("", "", "", "").orpheusOff.Load() {
		t.Fatal("orpheus must be off without a Groq key")
	}
}
