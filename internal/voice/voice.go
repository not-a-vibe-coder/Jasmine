// Package voice handles voice notes: transcribing what people say (Groq Whisper, Gemini
// fallback) and speaking replies (Groq Orpheus when available, Microsoft Edge neural voices
// otherwise).
package voice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wujunwei928/edge-tts-go/edge_tts"
)

const (
	DefaultEdgeVoice    = "en-US-AvaMultilingualNeural"
	DefaultOrpheusVoice = "tara"
	whisperModel        = "whisper-large-v3-turbo"
	orpheusModel        = "canopylabs/orpheus-v1-english"
	maxSpeechChars      = 900
)

// Speech is synthesized audio ready for Telegram's sendVoice.
type Speech struct {
	Data     []byte
	MIMEType string // audio/ogg (Opus) or audio/mpeg
	FileName string
	Provider string
}

type Service struct {
	groqKey      string
	geminiKey    string
	edgeVoice    string
	orpheusVoice string
	groqBaseURL  string
	httpClient   *http.Client
	// Orpheus needs a one-time terms acceptance on the Groq account and ffmpeg to turn
	// its WAV into Opus. Once it fails for either reason it stays off for this process.
	orpheusOff atomic.Bool
}

func NewService(groqKey, geminiKey, edgeVoice, orpheusVoice string) *Service {
	if edgeVoice == "" {
		edgeVoice = DefaultEdgeVoice
	}
	if orpheusVoice == "" {
		orpheusVoice = DefaultOrpheusVoice
	}
	s := &Service{
		groqKey:      groqKey,
		geminiKey:    geminiKey,
		edgeVoice:    edgeVoice,
		orpheusVoice: orpheusVoice,
		groqBaseURL:  "https://api.groq.com/openai/v1",
		httpClient:   &http.Client{Timeout: 60 * time.Second},
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil || groqKey == "" {
		s.orpheusOff.Store(true)
	}
	return s
}

// Transcribe turns a voice note into text.
func (s *Service) Transcribe(ctx context.Context, audio []byte, fileName, mimeType string) (string, error) {
	if len(audio) == 0 {
		return "", fmt.Errorf("empty audio")
	}
	if fileName == "" {
		fileName = "voice.ogg"
	}
	if mimeType == "" {
		mimeType = "audio/ogg"
	}
	var errs []string
	if s.groqKey != "" {
		text, err := s.transcribeGroq(ctx, audio, fileName)
		if err == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
		errs = append(errs, fmt.Sprintf("groq: %v", err))
	}
	if s.geminiKey != "" {
		text, err := s.transcribeGemini(ctx, audio, mimeType)
		if err == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), nil
		}
		errs = append(errs, fmt.Sprintf("gemini: %v", err))
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("no transcription provider configured")
	}
	return "", fmt.Errorf("transcription failed: %s", strings.Join(errs, "; "))
}

func (s *Service) transcribeGroq(ctx context.Context, audio []byte, fileName string) (string, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("file", fileName)
	if err != nil {
		return "", err
	}
	_, _ = part.Write(audio)
	_ = w.WriteField("model", whisperModel)
	_ = w.WriteField("response_format", "json")
	_ = w.WriteField("temperature", "0")
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.groqBaseURL+"/audio/transcriptions", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.groqKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

func (s *Service) transcribeGemini(ctx context.Context, audio []byte, mimeType string) (string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{{
			"parts": []map[string]interface{}{
				{"inlineData": map[string]string{"mimeType": mimeType, "data": base64.StdEncoding.EncodeToString(audio)}},
				{"text": "Transcribe the speech in this audio exactly. Output only the transcription, nothing else."},
			},
		}},
		"generationConfig": map[string]interface{}{"temperature": 0, "maxOutputTokens": 1000},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash-lite:generateContent", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", s.geminiKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty transcription")
	}
	return out.Candidates[0].Content.Parts[0].Text, nil
}

// Speak synthesizes a voice note. Orpheus is tried first because it is expressive
// (it can laugh and sigh via <laugh>, <sigh> tags); Edge TTS is the reliable fallback.
func (s *Service) Speak(ctx context.Context, text string) (*Speech, error) {
	if !s.orpheusOff.Load() {
		if sp, err := s.speakOrpheus(ctx, text); err == nil {
			return sp, nil
		} else {
			log.Printf("[Voice] Orpheus unavailable, using Edge TTS: %v", err)
			if strings.Contains(err.Error(), "terms") || strings.Contains(err.Error(), "ffmpeg") {
				s.orpheusOff.Store(true)
			}
		}
	}
	clean := CleanForSpeech(text, false)
	if clean == "" {
		return nil, fmt.Errorf("nothing speakable in the text")
	}
	return s.speakEdge(ctx, clean)
}

// SpeakMP3 always returns MP3 (Edge TTS), which phone systems and ffmpeg players accept.
// Calls use it because it is faster than Orpheus and Twilio can't play Opus.
func (s *Service) SpeakMP3(ctx context.Context, text string) (*Speech, error) {
	clean := CleanForSpeech(text, false)
	if clean == "" {
		return nil, fmt.Errorf("nothing speakable in the text")
	}
	return s.speakEdge(ctx, clean)
}

func (s *Service) speakOrpheus(ctx context.Context, text string) (*Speech, error) {
	clean := CleanForSpeech(text, true)
	if clean == "" {
		return nil, fmt.Errorf("nothing speakable in the text")
	}
	payload, _ := json.Marshal(map[string]string{
		"model": orpheusModel, "voice": s.orpheusVoice, "input": clean, "response_format": "wav",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.groqBaseURL+"/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.groqKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	wav, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("orpheus status %d: %s", resp.StatusCode, truncate(string(wav), 200))
	}
	ogg, err := wavToOpus(ctx, wav)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	return &Speech{Data: ogg, MIMEType: "audio/ogg", FileName: "jasmine.ogg", Provider: "orpheus"}, nil
}

func wavToOpus(ctx context.Context, wav []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-c:a", "libopus", "-b:a", "48k", "-f", "ogg", "pipe:1")
	cmd.Stdin = bytes.NewReader(wav)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, truncate(stderr.String(), 200))
	}
	return out.Bytes(), nil
}

func (s *Service) speakEdge(ctx context.Context, text string) (*Speech, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := edge_tts.NewCommunicate(text,
			edge_tts.SetVoice(s.edgeVoice),
			edge_tts.SetOutputFormat(edge_tts.OutputFormatMP3HQ),
			edge_tts.SetRate("+4%"))
		if err != nil {
			ch <- result{nil, err}
			return
		}
		data, err := c.Stream()
		ch <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("edge tts: %w", r.err)
		}
		if len(r.data) == 0 {
			return nil, fmt.Errorf("edge tts returned no audio")
		}
		return &Speech{Data: r.data, MIMEType: "audio/mpeg", FileName: "jasmine.mp3", Provider: "edge:" + s.edgeVoice}, nil
	}
}

var (
	speechURLRe      = regexp.MustCompile(`https?://\S+`)
	speechMarkdownRe = regexp.MustCompile("[*_`#>~|]+")
	speechEmojiRe    = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}\x{200D}]`)
	speechTagRe      = regexp.MustCompile(`<(laugh|chuckle|sigh|cough|sniffle|groan|yawn|gasp)>`)
	speechSpaceRe    = regexp.MustCompile(`\s+`)
)

// CleanForSpeech strips links, markdown and emojis so the voice doesn't read them out.
// Orpheus understands emotion tags like <laugh>; for other engines they are removed.
func CleanForSpeech(text string, keepEmotionTags bool) string {
	s := speechURLRe.ReplaceAllString(text, "")
	if keepEmotionTags {
		// Shield tags from the markdown pass, which strips '>'.
		s = speechTagRe.ReplaceAllString(s, "\x00$1\x01")
	} else {
		s = speechTagRe.ReplaceAllString(s, "")
	}
	s = speechMarkdownRe.ReplaceAllString(s, "")
	s = speechEmojiRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("\x00", "<", "\x01", ">").Replace(s)
	s = strings.TrimSpace(speechSpaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > maxSpeechChars {
		s = string(r[:maxSpeechChars])
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
