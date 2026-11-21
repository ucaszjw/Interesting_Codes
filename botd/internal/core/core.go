// Package core holds the small set of shared types the QQ transport needs.
// It is a trimmed-down copy of cc-connect's core package so the transport can
// live in this project without depending on the old engine.
package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Platform is a messaging adapter.
type Platform interface {
	Name() string
	Start(handler MessageHandler) error
	Reply(ctx context.Context, replyCtx any, content string) error
	Send(ctx context.Context, replyCtx any, content string) error
	Stop() error
}

// MessageHandler receives normalized inbound messages.
type MessageHandler func(p Platform, msg *Message)

// ReplyContextReconstructor recreates a reply context from a session key, used
// by scheduled jobs that send without an inbound message.
type ReplyContextReconstructor interface {
	ReconstructReplyCtx(sessionKey string) (any, error)
}

// Message is a normalized inbound message.
type Message struct {
	SessionKey   string
	Platform     string
	MessageID    string
	Recalled     bool
	ChannelID    string
	UserID       string
	UserName     string
	ChatName     string
	Content      string
	Images       []ImageAttachment
	Files        []FileAttachment
	Audio        *AudioAttachment
	ExtraContent string
	ChannelKey   string
	ReplyCtx     any
	FromVoice    bool
	// Kept for transport compatibility; the chat harness does not use permission
	// modes, so these are inert.
	ModeOverride            string
	BlockPermissionApproval bool
}

// ImageAttachment is an inbound image.
type ImageAttachment struct {
	MimeType string
	Data     []byte
	FileName string
	// Sticker marks a meme/market-face image rather than a normal photo.
	Sticker bool
	Label   string // e.g. "[/呲牙]" or "[动画表情]"
}

// FileAttachment is an inbound file.
type FileAttachment struct {
	MimeType string
	Data     []byte
	FileName string
}

// AudioAttachment is an inbound voice message.
type AudioAttachment struct {
	MimeType string
	Data     []byte
	Format   string
	Duration int
}

// ImageSender is implemented by platforms that can send images.
type ImageSender interface {
	SendImage(ctx context.Context, replyCtx any, img ImageAttachment) error
}

// ModeResetter is implemented by platforms that switch in-group behavior modes
// (such as /dict or /dark). It returns true once per switch so the harness can
// start a fresh conversation instead of judging old context by the new rules.
type ModeResetter interface {
	ConsumeModeReset(sessionKey string) bool
}

// AudioSender is implemented by platforms that can send voice messages.
type AudioSender interface {
	SendAudio(ctx context.Context, replyCtx any, audio []byte, format string) error
}

// VoiceSelector lets a platform pick a voice per conversation.
type VoiceSelector interface {
	SelectVoice(replyCtx any) string
}

// VoiceGate lets a platform veto a voice reply for policy reasons.
type VoiceGate interface {
	AllowVoice(replyCtx any, text string, fromVoice bool) bool
}

// StartTime is set once at process startup; messages older than it are ignored.
var StartTime = time.Now()

const dedupTTL = 60 * time.Second

// MessageDedup tracks recently seen message IDs.
type MessageDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// IsDuplicate reports whether msgID was already seen within the TTL window.
func (d *MessageDedup) IsDuplicate(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) > dedupTTL {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[msgID]; ok {
		return true
	}
	d.seen[msgID] = now
	return false
}

// IsOldMessage reports whether msgTime predates process start.
func IsOldMessage(msgTime time.Time) bool {
	return msgTime.Before(StartTime.Add(-2 * time.Second))
}

// CheckAllowFrom warns when allow_from is empty.
func CheckAllowFrom(platform, allowFrom string) {
	if strings.TrimSpace(allowFrom) == "" {
		slog.Warn("allow_from is not set — all users are permitted; set it to restrict access",
			"platform", platform)
	}
}

// ConvertAudioToMP3 converts audio to MP3 with ffmpeg.
func ConvertAudioToMP3(ctx context.Context, audio []byte, srcFormat string) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: install ffmpeg to enable voice messages")
	}
	var cmd *exec.Cmd
	if srcFormat == "amr" || srcFormat == "silk" {
		cmd = exec.CommandContext(ctx, ffmpegPath,
			"-f", srcFormat, "-i", "pipe:0", "-f", "mp3", "-ac", "1", "-ar", "16000", "-y", "pipe:1")
	} else {
		cmd = exec.CommandContext(ctx, ffmpegPath,
			"-i", "pipe:0", "-f", "mp3", "-ac", "1", "-ar", "16000", "-y", "pipe:1")
	}
	cmd.Stdin = bytes.NewReader(audio)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// builtinCommands are the control commands the QQ transport treats as commands
// rather than chat (they bypass the reply throttle).
var builtinCommands = [][]string{
	{"new"}, {"reset"}, {"persona"}, {"scope"}, {"game"}, {"bots"},
	{"mute"}, {"unmute"}, {"help"}, {"status"}, {"list", "sessions"},
	{"switch"}, {"name", "rename"}, {"facts"}, {"remember"}, {"forget"}, {"dict"}, {"dark"},
	{"emo"}, {"tts"}, {"compress", "compact"},
}

// IsBuiltinCommand reports whether raw names a built-in command.
func IsBuiltinCommand(raw string) bool {
	parts := strings.Fields(raw)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	for _, names := range builtinCommands {
		for _, n := range names {
			if cmd == n || strings.HasPrefix(n, cmd) {
				return true
			}
		}
	}
	return false
}

// PlatformFactory creates a platform from config options.
type PlatformFactory func(opts map[string]any) (Platform, error)

var platformFactories = map[string]PlatformFactory{}

// RegisterPlatform registers a platform factory (kept for the copied init()).
func RegisterPlatform(name string, factory PlatformFactory) {
	platformFactories[name] = factory
}
