package calls

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Twilio places phone calls. The conversation runs on TwiML webhooks: Jasmine's speech
// is played from /calls/audio, and Twilio's <Gather input="speech"> transcribes replies.
type Twilio struct {
	accountSID string
	authToken  string
	from       string
	speechLang string
	apiBase    string
	httpClient *http.Client
}

func NewTwilio(accountSID, authToken, from, speechLang string) *Twilio {
	if accountSID == "" || authToken == "" || from == "" {
		return nil
	}
	if speechLang == "" {
		speechLang = "en-US"
	}
	return &Twilio{accountSID, authToken, from, speechLang, "https://api.twilio.com", &http.Client{Timeout: 20 * time.Second}}
}

func (t *Twilio) Dial(ctx context.Context, to, answerURL, statusURL string) error {
	form := url.Values{
		"To":                  {to},
		"From":                {t.from},
		"Url":                 {answerURL},
		"Method":              {"POST"},
		"StatusCallback":      {statusURL},
		"StatusCallbackEvent": {"completed"},
		"Timeout":             {"30"},
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Calls.json", t.apiBase, t.accountSID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(t.accountSID, t.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message != "" {
			return fmt.Errorf("twilio %d: %s", e.Code, e.Message)
		}
		return fmt.Errorf("twilio status %d", resp.StatusCode)
	}
	return nil
}

// validSignature checks X-Twilio-Signature so nobody else can drive her calls.
// https://www.twilio.com/docs/usage/security#validating-requests
func (t *Twilio) validSignature(r *http.Request, publicURL string) bool {
	sig := r.Header.Get("X-Twilio-Signature")
	if sig == "" {
		return false
	}
	var sb strings.Builder
	sb.WriteString(publicURL + r.URL.RequestURI())
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range r.PostForm[k] {
			sb.WriteString(k + v)
		}
	}
	mac := hmac.New(sha1.New, []byte(t.authToken))
	mac.Write([]byte(sb.String()))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// TwilioHandler serves /calls/twilio/{voice,gather,status}.
func (m *Manager) TwilioHandler(w http.ResponseWriter, r *http.Request) {
	if m.Twilio == nil || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !m.Twilio.validSignature(r, m.publicURL) {
		log.Printf("[Calls] rejected Twilio request with a bad signature: %s", r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s := m.session(r.URL.Query().Get("call"))
	action := strings.TrimPrefix(r.URL.Path, "/calls/twilio/")

	if action == "status" {
		if s != nil {
			outcome := r.PostForm.Get("CallStatus") // completed, busy, no-answer, failed, canceled
			if outcome == "canceled" {
				outcome = "no-answer"
			}
			if outcome == "completed" && !s.Answered() {
				outcome = "no-answer"
			}
			m.finish(s, outcome)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/xml")
	if s == nil {
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Hangup/></Response>`)
		return
	}

	switch action {
	case "voice": // they picked up
		s.mu.Lock()
		s.answered = true
		opening := s.openingURL
		s.mu.Unlock()
		_, _ = io.WriteString(w, m.twiml(s, opening, false))

	case "gather":
		heard := strings.TrimSpace(r.PostForm.Get("SpeechResult"))
		if heard == "" {
			s.mu.Lock()
			s.silences++
			silences := s.silences
			s.mu.Unlock()
			if silences >= 2 {
				_, _ = io.WriteString(w, m.twimlSay(s, "okay, i'll let you go. talk soon.", true))
				return
			}
			_, _ = io.WriteString(w, m.twimlSay(s, "are you still there?", false))
			return
		}
		s.mu.Lock()
		s.silences = 0
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		text, audio, hangup := m.speak(ctx, s, heard)
		if len(audio) == 0 {
			_, _ = io.WriteString(w, m.twimlSay(s, text, hangup))
			return
		}
		_, _ = io.WriteString(w, m.twiml(s, m.storeAudio(audio), hangup))

	default:
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Hangup/></Response>`)
	}
}

// twiml plays a clip, then listens (the clip sits inside <Gather> so they can interrupt).
func (m *Manager) twiml(s *Session, audioURL string, hangup bool) string {
	play := fmt.Sprintf("<Play>%s</Play>", html.EscapeString(audioURL))
	if hangup {
		return `<?xml version="1.0" encoding="UTF-8"?><Response>` + play + `<Hangup/></Response>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?><Response>` + m.gather(s, play) + `</Response>`
}

// twimlSay is the fallback when our own voice couldn't be synthesized.
func (m *Manager) twimlSay(s *Session, text string, hangup bool) string {
	say := fmt.Sprintf(`<Say voice="Polly.Joanna-Neural">%s</Say>`, html.EscapeString(text))
	if hangup {
		return `<?xml version="1.0" encoding="UTF-8"?><Response>` + say + `<Hangup/></Response>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?><Response>` + m.gather(s, say) + `</Response>`
}

func (m *Manager) gather(s *Session, inner string) string {
	action := html.EscapeString(m.publicURL + "/calls/twilio/gather?call=" + s.ID)
	// If they say nothing, Twilio falls through to the Redirect, which arrives with no
	// SpeechResult and counts as a silence.
	return fmt.Sprintf(`<Gather input="speech" action="%s" method="POST" speechTimeout="auto" timeout="7" language="%s">%s</Gather><Redirect method="POST">%s</Redirect>`,
		action, m.Twilio.speechLang, inner, action)
}
