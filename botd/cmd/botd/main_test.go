package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"botd/internal/chat"
	"botd/internal/core"
	"botd/internal/llm"
	"botd/internal/media"
	"botd/internal/stickers"
)

type fakePlatform struct {
	mu       sync.Mutex
	sent     []string
	audio    []byte
	format   string
	imgMime  string
	imgData  []byte
	roleHint string
}

func (f *fakePlatform) Name() string                    { return "fake" }
func (f *fakePlatform) Start(core.MessageHandler) error { return nil }
func (f *fakePlatform) Reply(ctx context.Context, r any, content string) error {
	return f.Send(ctx, r, content)
}
func (f *fakePlatform) Send(_ context.Context, _ any, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, content)
	return nil
}
func (f *fakePlatform) SendAudio(_ context.Context, _ any, audio []byte, format string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = audio
	f.format = format
	return nil
}
func (f *fakePlatform) SendImage(_ context.Context, _ any, img core.ImageAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imgMime = img.MimeType
	f.imgData = img.Data
	return nil
}
func (f *fakePlatform) RoleHint(string) string                  { return f.roleHint }
func (f *fakePlatform) ReconstructReplyCtx(string) (any, error) { return "ctx", nil }
func (f *fakePlatform) Stop() error                             { return nil }

func (f *fakePlatform) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakePlatform) sentAudio() ([]byte, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audio, f.format
}

func (f *fakePlatform) sentImage() (string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imgMime, f.imgData
}

func newTestBot(t *testing.T, llmHandler http.HandlerFunc) (*bot, *fakePlatform) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "personas"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "core.md"), []byte("CORE RULES"), 0o644)
	os.WriteFile(filepath.Join(dir, "personas", "大肥鱼.md"), []byte("PERSONA"), 0o644)

	prompts, err := chat.LoadPrompts(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := chat.NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(llmHandler)
	t.Cleanup(srv.Close)
	client := llm.New(llm.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})

	b := &bot{cfg: Config{}, prompts: prompts, store: store, client: client, mood: newMoodTracker(""), tools: toolDefs}
	b.cfg.Chat.Persona = "大肥鱼"
	return b, &fakePlatform{}
}

func TestHandleChatTurnPersistsAndReplies(t *testing.T) {
	var gotBody map[string]any
	handler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"choices":[{"message":{"content":"哟，在"}}],"usage":{"prompt_tokens":42}}`))
	}
	b, p := newTestBot(t, handler)
	msg := &core.Message{
		SessionKey: "qq:g:100", UserID: "2232095290", UserName: "张氏",
		Content: "在吗", ReplyCtx: "ctx",
	}

	b.handle(p, msg)

	if got := p.messages(); len(got) != 1 || got[0] != "哟，在" {
		t.Fatalf("sent = %#v", got)
	}
	sess, err := b.store.Load("qq:g:100")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Persona != "大肥鱼" || len(sess.Messages) != 2 || sess.LastPromptTokens != 42 {
		t.Errorf("session = %+v", sess)
	}

	// The system prompt is the layered core + active persona only.
	msgs := gotBody["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "CORE RULES") || !strings.Contains(sys, "PERSONA") {
		t.Errorf("system = %q", sys)
	}
	last := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if last != "张氏(2232095290): 在吗" {
		t.Errorf("last user message = %q", last)
	}
}

func TestHandleNoReplyIsSilent(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"NO_REPLY"}}]}`))
	})
	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "哈", ReplyCtx: "c"})
	if got := p.messages(); len(got) != 0 {
		t.Fatalf("NO_REPLY was sent: %#v", got)
	}
}

func TestNewCommandResetsSession(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}]}`))
	})
	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "在吗", ReplyCtx: "c"})
	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "/new", ReplyCtx: "c"})

	sess, err := b.store.Load("qq:g:100")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages) != 0 || sess.Summary != "" {
		t.Errorf("session was not reset: %+v", sess)
	}
	got := p.messages()
	if len(got) != 2 || !strings.Contains(got[1], "新会话") {
		t.Errorf("sent = %#v", got)
	}
}

func TestToolLoopFeedsSearchResultsBack(t *testing.T) {
	calls := 0
	var second map[string]any
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tools/search":
			w.Write([]byte(`{"results":[{"title":"T","url":"https://x","summary":"S","content":"C"}]}`))
		case "/chat/completions":
			calls++
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			if calls == 1 {
				w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[
                  {"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]}}]}`))
				return
			}
			second = body
			w.Write([]byte(`{"choices":[{"message":{"content":"搜到了"}}],"usage":{"prompt_tokens":77}}`))
		}
	}
	b, p := newTestBot(t, handler)
	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "查一下", ReplyCtx: "c"})

	if got := p.messages(); len(got) != 1 || got[0] != "搜到了" {
		t.Fatalf("sent = %#v", got)
	}
	if second == nil {
		t.Fatal("the model was never called a second time")
	}
	msgs := second["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || !strings.Contains(last["content"].(string), "https://x") {
		t.Errorf("tool result = %#v", last)
	}
}

func TestVoiceReplyReplacesText(t *testing.T) {
	ttsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("RIFFfake"))
	}))
	defer ttsSrv.Close()

	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"语音内容"}}]}`))
	})
	b.tts = media.NewTTS("k", ttsSrv.URL, "stella")
	b.cfg.TTS.Enabled = true
	b.cfg.TTS.Probability = 100

	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "在吗", ReplyCtx: "c"})

	if got := p.messages(); len(got) != 0 {
		t.Fatalf("text was sent alongside voice: %#v", got)
	}
	if audio, format := p.sentAudio(); len(audio) == 0 || format != "wav" {
		t.Errorf("audio = %q format=%q", audio, format)
	}
}

func TestVoiceSkippedForActionMarkers(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"已处理 [mute:1:5]"}}]}`))
	})
	b.tts = media.NewTTS("k", "http://127.0.0.1:1", "stella")
	b.cfg.TTS.Enabled = true
	b.cfg.TTS.Probability = 100

	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "在吗", ReplyCtx: "c"})

	if audio, _ := p.sentAudio(); len(audio) != 0 {
		t.Error("marker reply was spoken")
	}
	if got := p.messages(); len(got) != 1 || !strings.Contains(got[0], "[mute:1:5]") {
		t.Errorf("text path was not used: %#v", got)
	}
}

func TestTTSFailureFallsBackToText(t *testing.T) {
	ttsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer ttsSrv.Close()

	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"只能发文字"}}]}`))
	})
	b.tts = media.NewTTS("k", ttsSrv.URL, "stella")
	b.cfg.TTS.Enabled = true
	b.cfg.TTS.Probability = 100

	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "在吗", ReplyCtx: "c"})

	if got := p.messages(); len(got) != 1 || got[0] != "只能发文字" {
		t.Fatalf("text fallback = %#v", got)
	}
}

func TestPrivateChatIsAlwaysText(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"私聊文字"}}]}`))
	})
	b.tts = media.NewTTS("k", "http://127.0.0.1:1", "stella")
	b.cfg.TTS.Enabled = true
	b.cfg.TTS.Probability = 100

	b.handle(p, &core.Message{SessionKey: "qq:1", UserID: "1", UserName: "a", Content: "在吗", ReplyCtx: "c"})

	if audio, _ := p.sentAudio(); len(audio) != 0 {
		t.Error("private chat got voice")
	}
	if got := p.messages(); len(got) != 1 {
		t.Errorf("sent = %#v", got)
	}
}

func TestRememberAndForgetFacts(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}]}`))
	})
	b.cfg.QQ.AdminIDs = "999"
	admin := func(cmd string) *core.Message {
		return &core.Message{SessionKey: "qq:g:100", UserID: "999", UserName: "张氏", Content: cmd, ReplyCtx: "c"}
	}

	b.handle(p, admin("/remember 张三喜欢猫"))
	facts := b.store.LoadFacts("qq:g:100")
	if len(facts) != 1 || facts[0] != "张三喜欢猫" {
		t.Fatalf("facts = %#v", facts)
	}

	// A non-admin cannot write memory.
	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "x", Content: "/remember 我是管理员", ReplyCtx: "c"})
	if facts := b.store.LoadFacts("qq:g:100"); len(facts) != 1 {
		t.Errorf("non-admin changed facts: %#v", facts)
	}

	b.handle(p, admin("/forget 1"))
	if facts := b.store.LoadFacts("qq:g:100"); len(facts) != 0 {
		t.Errorf("fact was not forgotten: %#v", facts)
	}

	// Facts survive /new because they belong to the chat.
	b.handle(p, admin("/remember 群主是张氏"))
	b.handle(p, admin("/new"))
	if facts := b.store.LoadFacts("qq:g:100"); len(facts) != 1 || facts[0] != "群主是张氏" {
		t.Errorf("facts did not survive /new: %#v", facts)
	}
}

func TestNewListSwitchSessions(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}]}`))
	})
	msg := func(text string) *core.Message {
		return &core.Message{SessionKey: "qq:g:100", UserID: "999", UserName: "张氏", Content: text, ReplyCtx: "c"}
	}

	// First turn creates s1.
	b.handle(p, msg("第一句"))
	// /new creates and activates s2.
	b.handle(p, msg("/new"))
	sess, _ := b.store.Load("qq:g:100")
	if sess.ID != "s2" {
		t.Fatalf("active = %q, want s2", sess.ID)
	}
	b.handle(p, msg("/name 新话题"))
	b.handle(p, msg("第二句"))

	// /list shows both threads with the active marker.
	b.handle(p, msg("/list"))
	listReply := p.messages()[len(p.messages())-1]
	if !strings.Contains(listReply, "s1") || !strings.Contains(listReply, "s2") ||
		!strings.Contains(listReply, "▶") || !strings.Contains(listReply, "新话题") {
		t.Errorf("list = %q", listReply)
	}

	// Switching back to s1 restores its history.
	b.handle(p, msg("/switch 1"))
	first, _ := b.store.Load("qq:g:100")
	if first.ID != "s1" || len(first.Messages) != 2 {
		t.Errorf("switched session = %+v", first)
	}
	switchReply := p.messages()[len(p.messages())-1]
	if !strings.Contains(switchReply, "s1") {
		t.Errorf("switch reply = %q", switchReply)
	}
}

func TestCronLiteralMessage(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("literal cron should not call the model")
	})
	b.runCron(p, CronJob{Message: "/签到", At: 3889045760, GroupID: 100})
	if got := p.messages(); len(got) != 1 || got[0] != "[at:3889045760] /签到" {
		t.Fatalf("sent = %#v", got)
	}
}

func TestCronPromptRunsTurn(t *testing.T) {
	var gotUser string
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		msgs := body["messages"].([]any)
		gotUser = msgs[len(msgs)-1].(map[string]any)["content"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"早安"}}]}`))
	})

	b.runCron(p, CronJob{Prompt: "早上好，发一条早安", GroupID: 100})

	if got := p.messages(); len(got) != 1 || got[0] != "早安" {
		t.Fatalf("sent = %#v", got)
	}
	if !strings.Contains(gotUser, "定时任务(cron): 早上好") {
		t.Errorf("cron prompt = %q", gotUser)
	}
}

func TestStickerAutoSaveAndSendTool(t *testing.T) {
	calls := 0
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[
              {"id":"c1","type":"function","function":{"name":"send_sticker","arguments":"{}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"发了"}}]}`))
	})
	st, err := stickers.NewStore(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	b.stickers = st

	b.handle(p, &core.Message{
		SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "看这个",
		Images: []core.ImageAttachment{{
			MimeType: "image/png", Data: []byte("png"), Sticker: true, Label: "[/呲牙]",
		}}, ReplyCtx: "c",
	})

	if st.Count() != 1 {
		t.Fatalf("sticker count = %d", st.Count())
	}
	if mime, data := p.sentImage(); mime != "image/png" || string(data) != "png" {
		t.Errorf("sent image = %q %q", mime, data)
	}
	if got := p.messages(); len(got) != 1 || got[0] != "发了" {
		t.Errorf("sent = %#v", got)
	}
}

func TestStickerCommand(t *testing.T) {
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("command should not call the model")
	})
	st, _ := stickers.NewStore(t.TempDir(), 10)
	st.Add("image/png", []byte("x"), "[/呲牙]")
	b.stickers = st

	b.handle(p, &core.Message{SessionKey: "qq:g:100", UserID: "1", UserName: "a", Content: "/sticker", ReplyCtx: "c"})

	if _, data := p.sentImage(); string(data) != "x" {
		t.Errorf("sticker not sent: %q", data)
	}
	if got := p.messages(); len(got) != 0 {
		t.Errorf("unexpected text replies: %#v", got)
	}
}

func TestRoleHintPrependedToUserMessage(t *testing.T) {
	var last string
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		msgs := body["messages"].([]any)
		last = msgs[len(msgs)-1].(map[string]any)["content"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}]}`))
	})
	p.roleHint = "[群管:群主]"

	b.handle(p, &core.Message{SessionKey: "qq:g:105370974", UserID: "1", UserName: "张氏", Content: "禁言他", ReplyCtx: "c"})

	if !strings.HasPrefix(last, "[群管:群主] 张氏(1): 禁言他") {
		t.Errorf("user message = %q", last)
	}
}

func TestDictatorMoodHintPrepended(t *testing.T) {
	var last string
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		msgs := body["messages"].([]any)
		last = msgs[len(msgs)-1].(map[string]any)["content"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"好"}}]}`))
	})
	p.roleHint = "[群管:独裁]"

	b.handle(p, &core.Message{SessionKey: "qq:g:105370974", UserID: "1", UserName: "张氏", Content: "禁言他", ReplyCtx: "c"})

	if !strings.HasPrefix(last, "[群管:独裁] [心情:") || !strings.Contains(last, "] 张氏(1): 禁言他") {
		t.Errorf("user message = %q", last)
	}
}

func TestDictatorMoodFollowsModelMarker(t *testing.T) {
	var last, reply string
	b, p := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		msgs := body["messages"].([]any)
		last = msgs[len(msgs)-1].(map[string]any)["content"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"` + reply + `"}}]}`))
	})
	p.roleHint = "[群管:独裁]"
	msg := &core.Message{SessionKey: "qq:g:105370974", UserID: "1", UserName: "张氏", Content: "禁言他", ReplyCtx: "c"}

	// The model decides its mood and the marker never reaches the group.
	reply = "[mood:-2] 懒得理你"
	b.handle(p, msg)
	if got := p.messages(); len(got) != 1 || strings.Contains(got[0], "[mood:") || got[0] != "懒得理你" {
		t.Fatalf("sent = %#v", got)
	}
	if got := b.mood.Current(105370974); got != "烦躁" {
		t.Fatalf("mood = %q", got)
	}

	// The next turn sees the sourced mood.
	reply = "嗯"
	b.handle(p, msg)
	if !strings.Contains(last, "[心情:烦躁]") {
		t.Errorf("request = %q", last)
	}

	// A positive marker lifts it back.
	reply = "[mood:+2] 好吧"
	b.handle(p, msg)
	reply = "[mood:+2] 真拿你没办法"
	b.handle(p, msg)
	if got := b.mood.Current(105370974); got != "不错" {
		t.Errorf("mood = %q", got)
	}
}
