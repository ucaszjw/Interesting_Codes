// Package media holds voice synthesis helpers.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TTS synthesizes speech with DeepSeek's /v1/tools/tts endpoint, which returns
// the audio bytes directly (WAV by default).
type TTS struct {
	BaseURL string
	APIKey  string
	Voice   string // "stella" (default) or "tide"
	HTTP    *http.Client
}

// NewTTS creates a client.
func NewTTS(apiKey, baseURL, voice string) *TTS {
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1/tools/tts"
	}
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(base, "/tools/tts") {
		base += "/tools/tts"
	}
	if voice == "" {
		voice = "stella"
	}
	return &TTS{BaseURL: base, APIKey: apiKey, Voice: voice,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Synthesize returns WAV audio for text.
func (t *TTS) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", fmt.Errorf("tts: empty text")
	}
	body, _ := json.Marshal(map[string]any{
		"text":            text,
		"format":          "wav",
		"filter_markdown": true,
		"voice_id":        t.Voice,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("tts: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("tts: request: %w", err)
	}
	defer resp.Body.Close()
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("tts: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("tts API %d: %s", resp.StatusCode, truncate(string(audio), 200))
	}
	if len(audio) == 0 {
		return nil, "", fmt.Errorf("tts: empty audio")
	}
	return audio, "wav", nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
