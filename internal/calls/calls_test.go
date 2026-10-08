package calls

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBrain struct {
	mu       sync.Mutex
	replies  []string
	finished chan string
}

func (f *fakeBrain) Reply(_ context.Context, s *Session) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return strings.TrimSuffix(r, "[END]"), strings.HasSuffix(r, "[END]")
}
func (f *fakeBrain) SpeakMP3(_ context.Context, text string) ([]byte, error) {
	return []byte("MP3:" + text), nil
}
func (f *fakeBrain) Transcribe(_ context.Context, audio []byte, _, _ string) (string, error) {
	return "heard " + string(audio), nil
}
func (f *fakeBrain) Finished(s *Session, outcome string) { f.finished <- outcome }

func sign(token, fullURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(fullURL)
	for _, k := range keys {
		sb.WriteString(k + form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestPhoneConversation(t *testing.T) {
	// Fake Twilio REST API that records the dial request.
	var dialed url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "AC1" || p != "tok" {
			http.Error(w, `{"message":"auth"}`, 401)
			return
		}
		_ = r.ParseForm()
		dialed = r.PostForm
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"sid":"CA1"}`))
	}))
	defer api.Close()

	brain := &fakeBrain{replies: []string{"hi skipp, time to fetch water", "glad you got it, bye[END]"}, finished: make(chan string, 1)}
	const public = "https://jasmine.example"
	m := NewManager(brain, public)
	m.Twilio = NewTwilio("AC1", "tok", "+15550001111", "")
	m.Twilio.apiBase = api.URL

	s := &Session{Channel: ChannelPhone, Name: "Skipp", Purpose: "reminder: fetch water"}
	if err := m.Start(context.Background(), s, "+2348031234567"); err != nil {
		t.Fatal(err)
	}
	if dialed.Get("To") != "+2348031234567" || dialed.Get("Url") != public+"/calls/twilio/voice?call="+s.ID {
		t.Fatalf("dial form wrong: %v", dialed)
	}

	post := func(path string, form url.Values, good bool) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		sig := sign("tok", public+path, form)
		if !good {
			sig = "forged"
		}
		req.Header.Set("X-Twilio-Signature", sig)
		rec := httptest.NewRecorder()
		m.TwilioHandler(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, _ := post("/calls/twilio/voice?call="+s.ID, url.Values{"CallSid": {"CA1"}}, false); code != 403 {
		t.Fatalf("forged signature must be rejected, got %d", code)
	}
	code, twiml := post("/calls/twilio/voice?call="+s.ID, url.Values{"CallSid": {"CA1"}}, true)
	if code != 200 || !strings.Contains(twiml, "<Gather input=\"speech\"") || !strings.Contains(twiml, public+"/calls/audio/") {
		t.Fatalf("answer twiml: %d %s", code, twiml)
	}

	// The opening clip is fetchable from the audio URL.
	start := strings.Index(twiml, "/calls/audio/")
	audioPath := twiml[start : start+strings.Index(twiml[start:], "</Play>")]
	rec := httptest.NewRecorder()
	m.AudioHandler(rec, httptest.NewRequest(http.MethodGet, audioPath, nil))
	if rec.Body.String() != "MP3:hi skipp, time to fetch water" {
		t.Fatalf("audio clip: %q", rec.Body.String())
	}

	_, twiml = post("/calls/twilio/gather?call="+s.ID, url.Values{"SpeechResult": {"okay i'm going now"}}, true)
	if !strings.Contains(twiml, "<Hangup/>") {
		t.Fatalf("goodbye should hang up: %s", twiml)
	}
	turns := s.Turns()
	if len(turns) != 3 || turns[1].Text != "okay i'm going now" || turns[1].Role != "them" {
		t.Fatalf("turns: %+v", turns)
	}

	post("/calls/twilio/status?call="+s.ID, url.Values{"CallStatus": {"completed"}}, true)
	select {
	case got := <-brain.finished:
		if got != "completed" {
			t.Fatalf("outcome %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finished not called")
	}
}

func TestPhoneUnanswered(t *testing.T) {
	brain := &fakeBrain{replies: []string{"hey"}, finished: make(chan string, 1)}
	m := NewManager(brain, "https://j.example")
	m.Twilio = NewTwilio("AC1", "tok", "+1555", "")
	s := &Session{Channel: ChannelPhone}
	m.register(s)
	form := url.Values{"CallStatus": {"no-answer"}}
	path := "/calls/twilio/status?call=" + s.ID
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", sign("tok", "https://j.example"+path, form))
	m.TwilioHandler(httptest.NewRecorder(), req)
	if got := <-brain.finished; got != "no-answer" {
		t.Fatalf("outcome %q", got)
	}
}

func TestTelegramBridge(t *testing.T) {
	var dialBody map[string]string
	caller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Caller-Secret") != "sek" {
			http.Error(w, "no", 403)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&dialBody)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer caller.Close()

	brain := &fakeBrain{replies: []string{"hey tobi", "talk soon[END]"}, finished: make(chan string, 1)}
	m := NewManager(brain, "")
	dir := t.TempDir()
	m.Telegram = NewTelegramCaller(caller.URL, "sek", dir)
	s := &Session{Channel: ChannelTelegram, Name: "Tobi"}
	if err := m.Start(context.Background(), s, "@tobi"); err != nil {
		t.Fatal(err)
	}
	if dialBody["user"] != "@tobi" || dialBody["call_id"] != s.ID {
		t.Fatalf("dial body %v", dialBody)
	}
	if b, _ := os.ReadFile(dialBody["audio_path"]); string(b) != "MP3:hey tobi" {
		t.Fatalf("opening clip %q", b)
	}

	event := func(secret string, ev callerEvent) (int, callerReply) {
		body, _ := json.Marshal(ev)
		req := httptest.NewRequest(http.MethodPost, "/calls/tg/event", bytes.NewReader(body))
		req.Header.Set("X-Caller-Secret", secret)
		rec := httptest.NewRecorder()
		m.TelegramEventHandler(rec, req)
		var out callerReply
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := event("wrong", callerEvent{CallID: s.ID, Type: "answered"}); code != 403 {
		t.Fatalf("bad secret accepted: %d", code)
	}
	event("sek", callerEvent{CallID: s.ID, Type: "answered"})
	_, reply := event("sek", callerEvent{CallID: s.ID, Type: "speech", Audio: base64.StdEncoding.EncodeToString([]byte("bye jasmine"))})
	if !reply.Hangup || reply.AudioPath == "" {
		t.Fatalf("reply %+v", reply)
	}
	if turns := s.Turns(); turns[1].Text != "heard bye jasmine" {
		t.Fatalf("turns %+v", turns)
	}
	event("sek", callerEvent{CallID: s.ID, Type: "ended", Reason: "completed"})
	if got := <-brain.finished; got != "completed" {
		t.Fatalf("outcome %q", got)
	}

	// Declined before answering.
	s2 := &Session{Channel: ChannelTelegram, Name: "Ada"}
	brain.replies = []string{"hi"}
	_ = m.Start(context.Background(), s2, "@ada")
	event("sek", callerEvent{CallID: s2.ID, Type: "failed", Reason: "declined"})
	if got := <-brain.finished; got != "declined" {
		t.Fatalf("outcome %q", got)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"0803 123 4567":     "+2348031234567",
		"+234 803-123-4567": "+2348031234567",
		"2348031234567":     "+2348031234567",
		"0044 20 7946 0958": "+442079460958",
		"+1 (415) 555-2671": "+14155552671",
	}
	for in, want := range cases {
		if got, err := NormalizePhone(in, "234"); err != nil || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"12", "call me", "+1+2345678"} {
		if _, err := NormalizePhone(bad, "234"); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if MaskPhone("+2348031234567") != "•••4567" {
		t.Error(MaskPhone("+2348031234567"))
	}
}

func TestAllowCall(t *testing.T) {
	m := NewManager(&fakeBrain{}, "")
	for i := 0; i < 3; i++ {
		if !m.AllowCall(1, 3) {
			t.Fatal("under the limit")
		}
	}
	if m.AllowCall(1, 3) {
		t.Fatal("over the limit")
	}
	if !m.AllowCall(2, 3) {
		t.Fatal("limits are per person")
	}
}
