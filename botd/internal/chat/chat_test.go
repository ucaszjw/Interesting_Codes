package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPromptsLayering(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "core.md"), "CORE")
	writeFile(t, filepath.Join(dir, "groups", "100.md"), "GROUP")
	writeFile(t, filepath.Join(dir, "personas", "大肥鱼.md"), "PERSONA")

	p, err := LoadPrompts(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := p.System("100", "大肥鱼")
	if !strings.HasPrefix(got, "CORE") {
		t.Errorf("core must come first: %q", got)
	}
	if strings.Index(got, "GROUP") < strings.Index(got, "CORE") {
		t.Errorf("group must follow core: %q", got)
	}
	if strings.Index(got, "PERSONA") < strings.Index(got, "GROUP") {
		t.Errorf("persona must follow group: %q", got)
	}

	// A new session with another persona shares the same prefix bytes up to the
	// persona block.
	other := p.System("100", "猫娘")
	prefix := got[:strings.Index(got, "## 当前人设")]
	if !strings.HasPrefix(other, prefix) {
		t.Error("shared prefix changed between personas")
	}
}

func TestStoreSaveLoadReset(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load("qq:g:100")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Key != "qq:g:100" || len(sess.Messages) != 0 {
		t.Fatalf("fresh session = %+v", sess)
	}
	sess.Persona = "大肥鱼"
	sess.Messages = append(sess.Messages, Msg{Role: "user", Name: "张氏", Content: "在吗"})
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("qq:g:100")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Persona != "大肥鱼" || len(loaded.Messages) != 1 {
		t.Fatalf("loaded = %+v", loaded)
	}

	store.Reset(loaded)
	if len(loaded.Messages) != 0 || loaded.Summary != "" || loaded.Persona != "大肥鱼" {
		t.Errorf("reset kept the wrong state: %+v", loaded)
	}
}

type fakeSummarizer struct {
	prompt string
	out    string
}

func (f *fakeSummarizer) Summarize(_ context.Context, prompt string) (string, error) {
	f.prompt = prompt
	return f.out, nil
}

func TestCompressorRollsOldMessagesIntoSummary(t *testing.T) {
	f := &fakeSummarizer{out: "SUMMARY"}
	c := &Compressor{Summarizer: f, MaxTokens: 10, KeepRecent: 3, SummaryMaxChars: 100}
	sess := &Session{Key: "k", Summary: "OLD"}
	for i := 0; i < 20; i++ {
		sess.Messages = append(sess.Messages, Msg{Role: "user", Name: "a", Content: "hello"})
	}

	changed, err := c.Maybe(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected compression")
	}
	if len(sess.Messages) != 3 {
		t.Errorf("kept %d messages, want 3", len(sess.Messages))
	}
	if sess.Summary != "SUMMARY" {
		t.Errorf("summary = %q", sess.Summary)
	}
	if !strings.Contains(f.prompt, "OLD") || !strings.Contains(f.prompt, "a: hello") {
		t.Errorf("prompt missing context: %q", f.prompt)
	}
}

func TestCompressorSkipsUnderBudget(t *testing.T) {
	f := &fakeSummarizer{out: "SUMMARY"}
	c := &Compressor{Summarizer: f, MaxTokens: 100000, KeepRecent: 3}
	sess := &Session{Messages: []Msg{{Role: "user", Content: "hi"}}}
	changed, err := c.Maybe(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("compressed even though the session is small")
	}
}
