// Package calls lets Jasmine phone people and hold a spoken conversation, over the phone
// network (Twilio) or as a Telegram voice call (through a companion user account, since
// Telegram bots cannot place calls). Each call is a Session: Jasmine speaks, listens,
// transcribes, thinks and answers until someone says goodbye.
package calls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ChannelPhone    = "phone"
	ChannelTelegram = "telegram"

	maxCallLength = 6 * time.Minute
	audioTTL      = 20 * time.Minute
)

type Turn struct {
	Role string // "jasmine" or "them"
	Text string
}

// Session is one call, from dialing to hang-up.
type Session struct {
	ID        string
	Channel   string
	ChatID    int64 // chat to report back to
	ThreadID  int
	ReplyTo   int
	UserID    int64
	Name      string
	Username  string
	Purpose   string // why she is calling, e.g. "reminder: fetch water"
	Memories  string // what she remembers about them, recalled before dialing
	Requester string // who asked for the call, when not the callee
	// FallbackText is sent to the chat if nobody picks up (e.g. the reminder itself).
	FallbackText string

	mu       sync.Mutex
	turns    []Turn
	started  time.Time
	answered bool
	silences int
	ended    bool

	openingURL string // phone: the pre-rendered first line
}

func (s *Session) Turns() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Turn(nil), s.turns...)
}

func (s *Session) addTurn(role, text string) {
	s.mu.Lock()
	s.turns = append(s.turns, Turn{role, text})
	s.mu.Unlock()
}

func (s *Session) Answered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

// Brain is what the bot supplies: thinking, a voice, ears, and what to do afterwards.
type Brain interface {
	// Reply returns Jasmine's next line; hangup=true when the conversation is done.
	Reply(ctx context.Context, s *Session) (text string, hangup bool)
	SpeakMP3(ctx context.Context, text string) ([]byte, error)
	Transcribe(ctx context.Context, audio []byte, fileName, mime string) (string, error)
	// Finished runs once per call: outcome is "completed", "no-answer", "busy", "declined" or "failed".
	Finished(s *Session, outcome string)
}

type audioClip struct {
	data    []byte
	expires time.Time
}

type Manager struct {
	brain     Brain
	publicURL string // https://jasmine.onrender.com, for Twilio callbacks and audio
	Twilio    *Twilio
	Telegram  *TelegramCaller

	mu       sync.Mutex
	sessions map[string]*Session
	audio    map[string]audioClip
	usage    map[int64][]time.Time
}

func NewManager(brain Brain, publicURL string) *Manager {
	return &Manager{
		brain:     brain,
		publicURL: strings.TrimRight(publicURL, "/"),
		sessions:  map[string]*Session{},
		audio:     map[string]audioClip{},
		usage:     map[int64][]time.Time{},
	}
}

// Available reports which channels can place calls right now.
func (m *Manager) Available() (phone, telegram bool) {
	if m == nil {
		return false, false
	}
	return m.Twilio != nil && m.publicURL != "", m.Telegram != nil
}

// AllowCall rate-limits calls a person can trigger (calls cost money and interrupt people).
func (m *Manager) AllowCall(requesterID int64, perDay int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	recent := m.usage[requesterID][:0]
	for _, t := range m.usage[requesterID] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= perDay {
		m.usage[requesterID] = recent
		return false
	}
	m.usage[requesterID] = append(recent, time.Now())
	return true
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) register(s *Session) {
	s.ID = newID()
	s.started = time.Now()
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
}

func (m *Manager) session(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// storeAudio keeps a clip for Twilio to fetch and returns its public URL.
func (m *Manager) storeAudio(data []byte) string {
	token := newID()
	m.mu.Lock()
	now := time.Now()
	for k, v := range m.audio {
		if now.After(v.expires) {
			delete(m.audio, k)
		}
	}
	m.audio[token] = audioClip{data, now.Add(audioTTL)}
	m.mu.Unlock()
	return m.publicURL + "/calls/audio/" + token + ".mp3"
}

// AudioHandler serves synthesized speech. Tokens are random and short-lived.
func (m *Manager) AudioHandler(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calls/audio/"), ".mp3")
	m.mu.Lock()
	clip, ok := m.audio[token]
	m.mu.Unlock()
	if !ok || time.Now().After(clip.expires) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(clip.data)
}

// speak generates Jasmine's next line and its audio. An empty heard means she speaks first.
func (m *Manager) speak(ctx context.Context, s *Session, heard string) (text string, audio []byte, hangup bool) {
	if heard != "" {
		s.addTurn("them", heard)
	}
	text, hangup = m.brain.Reply(ctx, s)
	if strings.TrimSpace(text) == "" {
		text, hangup = "sorry, i lost you for a second. i'll text you instead.", true
	}
	s.addTurn("jasmine", text)
	if time.Since(s.started) > maxCallLength {
		hangup = true
	}
	audio, err := m.brain.SpeakMP3(ctx, text)
	if err != nil {
		log.Printf("[Calls] %s tts failed: %v", s.ID, err)
	}
	return text, audio, hangup
}

// finish reports the outcome exactly once and forgets the session shortly after.
func (m *Manager) finish(s *Session, outcome string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	log.Printf("[Calls] %s (%s to %s) ended: %s after %s", s.ID, s.Channel, s.Name, outcome, time.Since(s.started).Round(time.Second))
	go m.brain.Finished(s, outcome)
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
	})
}

// Start dials someone. For ChannelPhone, to is an E.164 number; for ChannelTelegram, a
// @username or numeric user ID.
func (m *Manager) Start(ctx context.Context, s *Session, to string) error {
	switch s.Channel {
	case ChannelPhone:
		if m.Twilio == nil || m.publicURL == "" {
			return fmt.Errorf("phone calls are not configured")
		}
	case ChannelTelegram:
		if m.Telegram == nil {
			return fmt.Errorf("telegram calls are not configured")
		}
	default:
		return fmt.Errorf("unknown call channel %q", s.Channel)
	}
	m.register(s)

	// Prepare the opening line before dialing so there's no dead air when they pick up.
	_, audio, _ := m.speak(ctx, s, "")
	if len(audio) == 0 {
		return fmt.Errorf("couldn't synthesize the opening line")
	}

	var err error
	if s.Channel == ChannelPhone {
		s.mu.Lock()
		s.openingURL = m.storeAudio(audio)
		s.mu.Unlock()
		err = m.Twilio.Dial(ctx, to, m.publicURL+"/calls/twilio/voice?call="+s.ID, m.publicURL+"/calls/twilio/status?call="+s.ID)
	} else {
		var path string
		path, err = m.Telegram.WriteClip(s.ID, audio)
		if err == nil {
			err = m.Telegram.Dial(ctx, s.ID, to, path)
		}
	}
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
		return err
	}
	log.Printf("[Calls] %s dialing %s via %s (%s)", s.ID, s.Name, s.Channel, s.Purpose)
	return nil
}
