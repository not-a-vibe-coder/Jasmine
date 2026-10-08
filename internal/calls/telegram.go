package calls

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// TelegramCaller talks to caller/caller.py, a small Python service logged into a normal
// Telegram account (bots can't place calls). It dials, plays the clips we write to a
// shared folder, and posts back what the other person said.
type TelegramCaller struct {
	url        string // e.g. http://127.0.0.1:8091
	secret     string
	clipDir    string
	httpClient *http.Client
}

func NewTelegramCaller(url, secret, clipDir string) *TelegramCaller {
	if url == "" || secret == "" {
		return nil
	}
	if clipDir == "" {
		clipDir = filepath.Join(os.TempDir(), "jasmine-calls")
	}
	_ = os.MkdirAll(clipDir, 0o700)
	return &TelegramCaller{url, secret, clipDir, &http.Client{Timeout: 30 * time.Second}}
}

// WriteClip saves audio where the caller process can read it.
func (t *TelegramCaller) WriteClip(callID string, audio []byte) (string, error) {
	path := filepath.Join(t.clipDir, fmt.Sprintf("%s-%d.mp3", callID, time.Now().UnixNano()))
	if err := os.WriteFile(path, audio, 0o600); err != nil {
		return "", err
	}
	time.AfterFunc(audioTTL, func() { _ = os.Remove(path) })
	return path, nil
}

func (t *TelegramCaller) Dial(ctx context.Context, callID, to, clipPath string) error {
	body, _ := json.Marshal(map[string]string{"call_id": callID, "user": to, "audio_path": clipPath})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url+"/call", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caller-Secret", t.secret)
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram caller unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("telegram caller: %s", bytes.TrimSpace(msg))
	}
	return nil
}

// callerEvent is what caller.py posts to /calls/tg/event.
type callerEvent struct {
	CallID string `json:"call_id"`
	Type   string `json:"type"` // answered, speech, silence, ended, failed
	Reason string `json:"reason,omitempty"`
	Audio  string `json:"audio,omitempty"` // base64 WAV for "speech"
}

type callerReply struct {
	AudioPath string `json:"audio_path,omitempty"`
	Hangup    bool   `json:"hangup"`
}

// TelegramEventHandler receives call progress and speech from caller.py.
func (m *Manager) TelegramEventHandler(w http.ResponseWriter, r *http.Request) {
	if m.Telegram == nil || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Caller-Secret")), []byte(m.Telegram.secret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var ev callerEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	s := m.session(ev.CallID)
	if s == nil {
		writeJSON(w, callerReply{Hangup: true})
		return
	}

	switch ev.Type {
	case "answered":
		log.Printf("[Calls] %s answered", s.ID)
		s.mu.Lock()
		s.answered = true
		s.mu.Unlock()
		writeJSON(w, callerReply{})

	case "ended", "failed":
		outcome := "completed"
		if !s.Answered() {
			outcome = "no-answer"
			switch ev.Reason {
			case "declined", "busy":
				outcome = ev.Reason
			case "", "no-answer", "missed":
			default:
				outcome = "failed"
				log.Printf("[Calls] telegram call %s failed: %s", s.ID, ev.Reason)
			}
		}
		m.finish(s, outcome)
		writeJSON(w, callerReply{})

	case "silence":
		s.mu.Lock()
		s.silences++
		silences := s.silences
		s.mu.Unlock()
		line, hangup := "are you still there?", false
		if silences >= 2 {
			line, hangup = "okay, i'll let you go. talk soon.", true
		}
		s.addTurn("jasmine", line)
		writeJSON(w, m.telegramClip(r.Context(), s, line, nil, hangup))

	case "speech":
		wav, err := base64.StdEncoding.DecodeString(ev.Audio)
		if err != nil || len(wav) == 0 {
			writeJSON(w, callerReply{})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		heard, err := m.brain.Transcribe(ctx, wav, "speech.wav", "audio/wav")
		if err != nil || len(heard) < 2 {
			log.Printf("[Calls] %s couldn't transcribe %d bytes of speech: %v", s.ID, len(wav), err)
			writeJSON(w, callerReply{}) // couldn't make it out; keep listening
			return
		}
		log.Printf("[Calls] %s heard %d chars", s.ID, len(heard))
		s.mu.Lock()
		s.silences = 0
		s.mu.Unlock()
		_, audio, hangup := m.speak(ctx, s, heard)
		writeJSON(w, m.telegramClip(ctx, s, "", audio, hangup))

	default:
		writeJSON(w, callerReply{})
	}
}

func (m *Manager) telegramClip(ctx context.Context, s *Session, text string, audio []byte, hangup bool) callerReply {
	if len(audio) == 0 && text != "" {
		audio, _ = m.brain.SpeakMP3(ctx, text)
	}
	if len(audio) == 0 {
		return callerReply{Hangup: hangup}
	}
	path, err := m.Telegram.WriteClip(s.ID, audio)
	if err != nil {
		return callerReply{Hangup: hangup}
	}
	return callerReply{AudioPath: path, Hangup: hangup}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TelegramTarget turns a username or numeric ID into what caller.py accepts.
func TelegramTarget(username string, userID int64) string {
	if username != "" {
		return "@" + username
	}
	return strconv.FormatInt(userID, 10)
}
