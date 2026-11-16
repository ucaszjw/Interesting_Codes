package chat

import (
	"context"
	"fmt"
	"strings"
)

// Summarizer turns a transcript (plus the previous summary) into a shorter one.
type Summarizer interface {
	Summarize(ctx context.Context, prompt string) (string, error)
}

// Compressor keeps a session's context bounded by rolling old turns into a
// summary. It only runs between turns, never mid-request.
type Compressor struct {
	Summarizer      Summarizer
	MaxTokens       int // compress when the prompt is larger than this
	KeepRecent      int // verbatim turns kept after compression
	SummaryMaxChars int
}

// Maybe compresses sess in place and reports whether anything changed.
func (c *Compressor) Maybe(ctx context.Context, sess *Session) (bool, error) {
	if c == nil || c.Summarizer == nil || len(sess.Messages) <= c.KeepRecent {
		return false, nil
	}

	tokens := sess.LastPromptTokens
	if tokens == 0 {
		tokens = estimateSessionTokens(sess)
	}
	if c.MaxTokens > 0 && tokens <= c.MaxTokens {
		return false, nil
	}

	keep := c.KeepRecent
	if keep < 1 {
		keep = 1
	}
	if keep >= len(sess.Messages) {
		return false, nil
	}
	split := len(sess.Messages) - keep
	old := sess.Messages[:split]
	recent := sess.Messages[split:]

	var b strings.Builder
	if sess.Summary != "" {
		b.WriteString("已有摘要：\n")
		b.WriteString(sess.Summary)
		b.WriteString("\n\n")
	}
	b.WriteString("新增记录：\n")
	for _, m := range old {
		who := m.Name
		if who == "" {
			who = m.Role
		}
		text := m.Content
		if m.Images > 0 {
			text += " [图片]"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, text)
	}
	b.WriteString("\n请把以上内容合并进去，输出不超过 " +
		fmt.Sprint(c.maxSummaryChars()) + " 字的摘要。")

	summary, err := c.Summarizer.Summarize(ctx, b.String())
	if err != nil {
		return false, fmt.Errorf("compress: summarize: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return false, fmt.Errorf("compress: empty summary")
	}
	sess.Summary = truncateRunes(summary, c.maxSummaryChars())
	sess.Messages = recent
	sess.LastPromptTokens = 0
	return true, nil
}

func (c *Compressor) maxSummaryChars() int {
	if c.SummaryMaxChars > 0 {
		return c.SummaryMaxChars
	}
	return 500
}

// estimateSessionTokens is a rough CJK-aware estimate used until the provider
// reports real usage.
func estimateSessionTokens(sess *Session) int {
	n := estimateTokens(sess.Summary)
	for _, f := range sess.Facts {
		n += estimateTokens(f)
	}
	for _, m := range sess.Messages {
		n += estimateTokens(m.Content) + 8
	}
	return n
}

func estimateTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return ascii/4 + wide*2/3 + 1
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// MemoryPrompt renders the summary and facts as a system block for the model.
func (s *Session) MemoryPrompt() string {
	var b strings.Builder
	if len(s.Facts) > 0 {
		b.WriteString("长期记忆：\n")
		for _, f := range s.Facts {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	if s.Summary != "" {
		b.WriteString("历史摘要：")
		b.WriteString(s.Summary)
	}
	return strings.TrimSpace(b.String())
}
