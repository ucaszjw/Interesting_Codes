package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"

	"github.com/gorilla/websocket"
)

func TestPlatform_Name(t *testing.T) {
	p := &Platform{}
	if got := p.Name(); got != "qq" {
		t.Errorf("Name() = %q, want %q", got, "qq")
	}
}

func TestNew_DefaultWSURL(t *testing.T) {
	p, err := New(map[string]any{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	if platform.wsURL != "ws://127.0.0.1:3001" {
		t.Errorf("wsURL = %q, want %q", platform.wsURL, "ws://127.0.0.1:3001")
	}
}

func TestNew_CustomWSURL(t *testing.T) {
	p, err := New(map[string]any{
		"ws_url": "ws://example.com:8080",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	if platform.wsURL != "ws://example.com:8080" {
		t.Errorf("wsURL = %q, want %q", platform.wsURL, "ws://example.com:8080")
	}
}

func TestNew_WithToken(t *testing.T) {
	p, err := New(map[string]any{
		"token": "my-secret-token",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	if platform.token != "my-secret-token" {
		t.Errorf("token = %q, want %q", platform.token, "my-secret-token")
	}
}

func TestNew_WithAllowFrom(t *testing.T) {
	p, err := New(map[string]any{
		"allow_from": "user1,user2,*",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	if platform.allowFrom != "user1,user2,*" {
		t.Errorf("allowFrom = %q, want %q", platform.allowFrom, "user1,user2,*")
	}
}

func TestNew_ShareSessionInChannel(t *testing.T) {
	p, err := New(map[string]any{
		"share_session_in_channel": true,
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	if !platform.shareSessionInChannel {
		t.Error("shareSessionInChannel = false, want true")
	}
}

// verify Platform implements core.Platform
var _ core.Platform = (*Platform)(nil)

// TestNew_NumericOptionsAcceptTOMLInt64 guards against silently ignoring every
// numeric option. config.toml is decoded into map[string]any by BurntSushi/toml,
// which yields int64, whereas Go callers pass int.
func TestNew_NumericOptionsAcceptTOMLInt64(t *testing.T) {
	p, err := New(map[string]any{
		"reply_probability":  int64(7),
		"reply_skip_penalty": int64(2),
		"reply_cold_boost":   int64(9),
		"reply_prob_min":     int64(3),
		"reply_prob_max":     int64(11),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	platform := p.(*Platform)
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"replyProbability", platform.replyProbability, 7},
		{"replySkipPenalty", platform.replySkipPenalty, 2},
		{"replyColdBoost", platform.replyColdBoost, 9},
		{"replyProbMin", platform.replyProbMin, 3},
		{"replyProbMax", platform.replyProbMax, 11},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestNew_ExplicitZeroReplyProbabilityKeepsZero documents that 0 means
// "probability disabled" and must not be replaced by the default of 30.
func TestNew_ExplicitZeroReplyProbabilityKeepsZero(t *testing.T) {
	p, err := New(map[string]any{"reply_probability": int64(0)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := p.(*Platform).replyProbability; got != 0 {
		t.Errorf("replyProbability = %d, want 0", got)
	}
}

func TestNew_ReplyProbabilityDefaultsWhenAbsent(t *testing.T) {
	p, err := New(map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := p.(*Platform).replyProbability; got != 30 {
		t.Errorf("replyProbability = %d, want default 30", got)
	}
}

func TestNew_VoiceProbability(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]any
		want int
	}{
		{"from toml int64", map[string]any{"voice_probability": int64(25)}, 25},
		{"absent is off", map[string]any{}, 0},
		{"negative clamps to 0", map[string]any{"voice_probability": int64(-5)}, 0},
		{"above 100 clamps", map[string]any{"voice_probability": int64(150)}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := p.(*Platform).voiceProbability; got != tc.want {
				t.Errorf("voiceProbability = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNew_PersonaVoices(t *testing.T) {
	p, err := New(map[string]any{
		"persona_voices": map[string]any{
			"猫娘": "zh-CN-XiaoyiNeural",
			"直男": "",
			"文豪": "zh-CN-YunxiNeural",
			"乱码": 42,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	voices := p.(*Platform).personaVoices
	if got := voices["猫娘"]; got != "zh-CN-XiaoyiNeural" {
		t.Errorf("猫娘 voice = %q, want %q", got, "zh-CN-XiaoyiNeural")
	}
	if _, ok := voices["直男"]; ok {
		t.Error("empty voice name should be dropped")
	}
	if _, ok := voices["乱码"]; ok {
		t.Error("non-string voice name should be dropped")
	}
	if len(voices) != 2 {
		t.Errorf("len(personaVoices) = %d, want 2", len(voices))
	}
}

func TestSelectVoice(t *testing.T) {
	p := &Platform{personaVoices: map[string]string{"猫娘": "voice-cat"}}

	if got := p.SelectVoice(&replyContext{persona: "猫娘"}); got != "voice-cat" {
		t.Errorf("SelectVoice(猫娘) = %q, want %q", got, "voice-cat")
	}
	if got := p.SelectVoice(&replyContext{persona: "没有配置的人设"}); got != "" {
		t.Errorf("unmapped persona should fall back to the global voice, got %q", got)
	}
	if got := p.SelectVoice(&replyContext{}); got != "" {
		t.Errorf("empty persona should return empty, got %q", got)
	}
	if got := p.SelectVoice("not a reply context"); got != "" {
		t.Errorf("unknown reply context should return empty, got %q", got)
	}
}

func TestAllowVoice(t *testing.T) {
	group := &replyContext{messageType: "group"}

	t.Run("private chats never get voice", func(t *testing.T) {
		p := &Platform{voiceProbability: 100}
		for _, rctx := range []any{&replyContext{messageType: "private"}, &replyContext{}, nil, "not a ctx"} {
			if p.AllowVoice(rctx, "", false) {
				t.Errorf("AllowVoice(%#v) = true, want false for non-group replies", rctx)
			}
		}
	})

	t.Run("no gating when probability is zero", func(t *testing.T) {
		p := &Platform{voiceProbability: 0}
		for i := 0; i < 100; i++ {
			if !p.AllowVoice(group, "", false) {
				t.Fatal("probability 0 should never gate")
			}
		}
	})

	t.Run("voice reply is never gated", func(t *testing.T) {
		p := &Platform{voiceProbability: 1}
		for i := 0; i < 100; i++ {
			if !p.AllowVoice(group, "", true) {
				t.Fatal("a reply to a voice message should always be spoken")
			}
		}
	})

	t.Run("probability 100 always allows", func(t *testing.T) {
		p := &Platform{voiceProbability: 100}
		for i := 0; i < 100; i++ {
			if !p.AllowVoice(group, "", false) {
				t.Fatal("probability 100 should never gate")
			}
		}
	})

	t.Run("probability 50 allows and refuses", func(t *testing.T) {
		p := &Platform{voiceProbability: 50}
		allowed, refused := 0, 0
		for i := 0; i < 1000; i++ {
			if p.AllowVoice(group, "", false) {
				allowed++
			} else {
				refused++
			}
		}
		if allowed == 0 || refused == 0 {
			t.Errorf("expected a mix of decisions, got allowed=%d refused=%d", allowed, refused)
		}
	})
}

func TestSendAudio_RejectsInvalidReplyContext(t *testing.T) {
	p := &Platform{}
	if err := p.SendAudio(context.Background(), "not a reply context", []byte("x"), "mp3"); err == nil {
		t.Error("expected an error for an invalid reply context")
	}
}

func TestSendAudio_RejectsEmptyAudio(t *testing.T) {
	p := &Platform{}
	rctx := &replyContext{messageType: "group", groupID: 1}
	if err := p.SendAudio(context.Background(), rctx, nil, "mp3"); err == nil {
		t.Error("expected an error for empty audio")
	}
}

func TestParseMessage_ReportsVoiceOrigin(t *testing.T) {
	p := &Platform{}

	voice := map[string]any{
		"message": []any{
			map[string]any{"type": "record", "data": map[string]any{"file": "flag_3s.amr"}},
		},
	}
	text, _, _, fromVoice := p.parseMessage(voice)
	if !fromVoice {
		t.Error("fromVoice = false, want true for a record segment")
	}
	if text != "[语音 3s]" {
		t.Errorf("text = %q, want %q", text, "[语音 3s]")
	}

	textMsg := map[string]any{
		"message": []any{
			map[string]any{"type": "text", "data": map[string]any{"text": "hi"}},
		},
	}
	if _, _, _, fromVoice := p.parseMessage(textMsg); fromVoice {
		t.Error("fromVoice = true, want false for a text message")
	}
}

func TestPersonaFor(t *testing.T) {
	p := &Platform{}
	if got := p.personaFor("qq:g:1"); got != defaultPersona {
		t.Errorf("personaFor with no override = %q, want %q", got, defaultPersona)
	}
	p.personaMap.Store("qq:g:1", "猫娘")
	if got := p.personaFor("qq:g:1"); got != "猫娘" {
		t.Errorf("personaFor = %q, want %q", got, "猫娘")
	}
	p.personaMap.Store("qq:g:2", "")
	if got := p.personaFor("qq:g:2"); got != defaultPersona {
		t.Errorf("empty override should fall back to %q, got %q", defaultPersona, got)
	}
}

func TestReconstructReplyCtx_CarriesPersona(t *testing.T) {
	p := &Platform{}
	p.personaMap.Store("qq:g:42", "文豪")

	got, err := p.ReconstructReplyCtx("qq:g:42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rctx, ok := got.(*replyContext)
	if !ok {
		t.Fatalf("got %T, want *replyContext", got)
	}
	if rctx.persona != "文豪" {
		t.Errorf("persona = %q, want %q", rctx.persona, "文豪")
	}

	// qq:{groupID}:{userID} form
	got, err = p.ReconstructReplyCtx("qq:42:7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rctx, ok = got.(*replyContext)
	if !ok {
		t.Fatalf("got %T, want *replyContext", got)
	}
	if rctx.messageType != "group" || rctx.groupID != 42 || rctx.userID != 7 {
		t.Errorf("unexpected reply context: %+v", rctx)
	}

	// private form keeps no persona
	got, err = p.ReconstructReplyCtx("qq:7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rctx, ok := got.(*replyContext); !ok || rctx.persona != "" {
		t.Errorf("private reply context should not carry a persona: %+v", rctx)
	}
}

// TestStart_FetchesSelfIDWithoutTimeout verifies that Start() completes
// promptly with selfID populated from the get_login_info OneBot API call.
// Regression for a bug where Start invoked callAPI BEFORE launching readLoop,
// so the API response had no consumer and callAPI always timed out after 15s
// — leaving selfID=0 and disabling the self-message filter in handleMessage.
func TestStart_FetchesSelfIDWithoutTimeout(t *testing.T) {
	const botUserID = 999999

	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			if req["action"] == "get_login_info" {
				echo, _ := req["echo"].(string)
				resp := map[string]any{
					"status":  "ok",
					"retcode": 0,
					"echo":    echo,
					"data":    map[string]any{"user_id": botUserID, "nickname": "TestBot"},
				}
				raw, _ := json.Marshal(resp)
				_ = c.WriteMessage(websocket.TextMessage, raw)
			}
		}
	}))
	defer ts.Close()

	p := &Platform{
		wsURL: "ws" + strings.TrimPrefix(ts.URL, "http"),
	}

	done := make(chan error, 1)
	go func() {
		done <- p.Start(func(core.Platform, *core.Message) {})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = p.Stop()
		t.Fatal("Start did not complete within 5s; readLoop likely starts after callAPI, so get_login_info never gets a response")
	}
	defer p.Stop()

	if p.selfID != botUserID {
		t.Errorf("selfID = %d, want %d (self-message filter would be disabled)", p.selfID, botUserID)
	}
}

// --- Chat scope ---

const fakeBotUserID = 999999

// fakeOneBot is a minimal OneBot v11 server: it answers the calls the qq adapter
// makes and records outgoing message params.
type fakeOneBot struct {
	ts   *httptest.Server
	mu   sync.Mutex
	conn *websocket.Conn
	sent []map[string]any

	// gorilla/websocket permits only one concurrent writer, and both the server
	// handler (API responses) and the test (incoming events) write to this conn.
	writeMu sync.Mutex
}

func (f *fakeOneBot) write(c *websocket.Conn, msg []byte) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return c.WriteMessage(websocket.TextMessage, msg)
}

func newFakeOneBot(t *testing.T) *fakeOneBot {
	t.Helper()
	f := &fakeOneBot{}
	upgrader := websocket.Upgrader{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = c
		f.mu.Unlock()
		defer c.Close()

		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			var data map[string]any
			switch req["action"] {
			case "get_login_info":
				data = map[string]any{"user_id": fakeBotUserID, "nickname": "TestBot"}
			case "get_group_info":
				data = map[string]any{"group_name": "测试群"}
			default:
				if params, ok := req["params"].(map[string]any); ok {
					f.mu.Lock()
					f.sent = append(f.sent, params)
					f.mu.Unlock()
				}
			}
			resp, _ := json.Marshal(map[string]any{
				"status": "ok", "retcode": 0, "echo": req["echo"], "data": data,
			})
			if err := f.write(c, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeOneBot) url() string { return "ws" + strings.TrimPrefix(f.ts.URL, "http") }

func (f *fakeOneBot) sendEvent(t *testing.T, event map[string]any) {
	t.Helper()
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c == nil {
		t.Fatal("fake OneBot has no client connection")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := f.write(c, raw); err != nil {
		t.Fatalf("send event: %v", err)
	}
}

func (f *fakeOneBot) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, p := range f.sent {
		if s, ok := p["message"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeOneBot) sentParams() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeOneBot) waitForText(t *testing.T, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range f.sentTexts() {
			if strings.Contains(s, substr) {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no outgoing message containing %q within %s; sent=%v", substr, timeout, f.sentTexts())
	return ""
}

// startQQ boots a Platform against a fake OneBot server.
func startQQ(t *testing.T, opts map[string]any, handler core.MessageHandler) (*Platform, *fakeOneBot) {
	t.Helper()
	f := newFakeOneBot(t)
	opts["ws_url"] = f.url()
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plat := p.(*Platform)
	if err := plat.Start(handler); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = plat.Stop() })
	return plat, f
}

func textEvent(msgType string, userID int64, text string) map[string]any {
	event := map[string]any{
		"post_type":    "message",
		"message_type": msgType,
		"user_id":      userID,
		"message_id":   time.Now().UnixNano(),
		"message": []any{
			map[string]any{"type": "text", "data": map[string]any{"text": text}},
		},
	}
	if msgType == "group" {
		event["group_id"] = int64(100)
	}
	return event
}

func TestParseChatScope(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"all", chatScopeAll},
		{"ALL", chatScopeAll},
		{"全部", chatScopeAll},
		{"group", chatScopeGroup},
		{"群聊", chatScopeGroup},
		{"private", chatScopePrivate},
		{"私聊", chatScopePrivate},
		{" private ", chatScopePrivate},
		{"garbage", ""},
		{"", ""},
	} {
		if got := parseChatScope(tc.in); got != tc.want {
			t.Errorf("parseChatScope(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsCommand(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"/scope", true},
		{"/scope group", true},
		{"/scopex", false},
		{"/persona", false},
		{"hey /scope", false},
		{"", false},
	} {
		if got := isCommand(tc.text, "/scope"); got != tc.want {
			t.Errorf("isCommand(%q, /scope) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestScopeAllows(t *testing.T) {
	for _, tc := range []struct {
		scope   string
		msgType string
		want    bool
	}{
		{chatScopeAll, "group", true},
		{chatScopeAll, "private", true},
		{chatScopeGroup, "group", true},
		{chatScopeGroup, "private", false},
		{chatScopePrivate, "private", true},
		{chatScopePrivate, "group", false},
	} {
		p := &Platform{chatScope: tc.scope}
		if got := p.scopeAllows(tc.msgType); got != tc.want {
			t.Errorf("scope=%s msgType=%s: scopeAllows = %v, want %v", tc.scope, tc.msgType, got, tc.want)
		}
	}
}

func TestScopeBlocks_LetsScopeCommandThrough(t *testing.T) {
	p := &Platform{chatScope: chatScopePrivate}

	if !p.scopeBlocks("group", "你好") {
		t.Error("plain group message should be blocked when scope is private")
	}
	if p.scopeBlocks("group", "/scope group") {
		t.Error("/scope must pass through so an admin can switch back")
	}
	if p.scopeBlocks("private", "你好") {
		t.Error("in-scope message should not be blocked")
	}
	if p.scopeBlocks("group", " /scope all") {
		t.Error("leading whitespace should still count as the command")
	}
}

func TestRawTextOf(t *testing.T) {
	payload := map[string]any{
		"message": []any{
			map[string]any{"type": "image", "data": map[string]any{"url": "http://example.invalid/x.png"}},
			map[string]any{"type": "text", "data": map[string]any{"text": "/scope group"}},
		},
	}
	if got := rawTextOf(payload); got != "/scope group" {
		t.Errorf("rawTextOf = %q, want %q", got, "/scope group")
	}

	raw := map[string]any{"raw_message": "[CQ:at,qq=1] hi"}
	if got := rawTextOf(raw); got != "hi" {
		t.Errorf("rawTextOf(raw_message) = %q, want %q", got, "hi")
	}
}

func TestNew_ChatScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]any
		want string
	}{
		{"absent defaults to all", map[string]any{}, chatScopeAll},
		{"invalid falls back to all", map[string]any{"chat_scope": "nonsense"}, chatScopeAll},
		{"group", map[string]any{"chat_scope": "group"}, chatScopeGroup},
		{"chinese alias", map[string]any{"chat_scope": "私聊"}, chatScopePrivate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := p.(*Platform).currentScope(); got != tc.want {
				t.Errorf("currentScope = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChatScope_PersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	opts := map[string]any{"cc_data_dir": dir, "cc_project": "my-proj"}

	if got := chatScopeStatePath(opts); !strings.HasSuffix(got, filepath.Join("qq", "my-proj", "chat_scope.json")) {
		t.Errorf("unexpected state path: %q", got)
	}

	if err := saveChatScope(chatScopeStatePath(opts), chatScopeGroup); err != nil {
		t.Fatalf("saveChatScope: %v", err)
	}
	if got := loadChatScope(chatScopeStatePath(opts)); got != chatScopeGroup {
		t.Errorf("loadChatScope = %q, want %q", got, chatScopeGroup)
	}
}

// TestNew_PersistedScopeWinsOverConfig documents that a /scope switch is sticky:
// the stored value beats the config default on the next start.
func TestNew_PersistedScopeWinsOverConfig(t *testing.T) {
	dir := t.TempDir()
	opts := map[string]any{"cc_data_dir": dir, "cc_project": "proj"}
	if err := saveChatScope(chatScopeStatePath(opts), chatScopePrivate); err != nil {
		t.Fatalf("saveChatScope: %v", err)
	}

	p, err := New(map[string]any{
		"chat_scope":  "group",
		"cc_data_dir": dir,
		"cc_project":  "proj",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := p.(*Platform).currentScope(); got != chatScopePrivate {
		t.Errorf("currentScope = %q, want persisted %q", got, chatScopePrivate)
	}
}

func TestChatScopeStatePath_EmptyWithoutInjection(t *testing.T) {
	if got := chatScopeStatePath(map[string]any{}); got != "" {
		t.Errorf("chatScopeStatePath = %q, want empty when data dir/project are missing", got)
	}
}

func TestSanitizePathPart(t *testing.T) {
	if got := sanitizePathPart("a/b:c"); got != "a_b_c" {
		t.Errorf("sanitizePathPart = %q, want %q", got, "a_b_c")
	}
	if got := sanitizePathPart("项目一"); got != "项目一" {
		t.Errorf("sanitizePathPart should keep non-ASCII letters, got %q", got)
	}
	if got := sanitizePathPart(""); got != "default" {
		t.Errorf("sanitizePathPart(\"\") = %q, want %q", got, "default")
	}
}

func TestHandleMessage_DropsOutOfScopeChats(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{"chat_scope": "private"}, func(_ core.Platform, m *core.Message) {
		got <- m
	})

	f.sendEvent(t, textEvent("group", 200, "你好"))

	select {
	case m := <-got:
		t.Fatalf("handler was called for an out-of-scope group message: %+v", m)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestHandleMessage_DeliversInScopeChats(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{
		"chat_scope":        "private",
		"reply_probability": 0,
	}, func(_ core.Platform, m *core.Message) {
		got <- m
	})

	f.sendEvent(t, textEvent("private", 200, "你好"))

	select {
	case m := <-got:
		if m.Content != "你好" {
			t.Errorf("Content = %q, want %q", m.Content, "你好")
		}
		if m.FromVoice {
			t.Error("FromVoice = true for a text message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not called for an in-scope private message")
	}
}

func TestScopeCommand_AdminOnly(t *testing.T) {
	plat, f := startQQ(t, map[string]any{
		"chat_scope": "all",
		"admin_ids":  "200",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 300, "/scope group"))
	f.waitForText(t, "只有管理员", 3*time.Second)

	if got := plat.currentScope(); got != chatScopeAll {
		t.Errorf("scope changed to %q by a non-admin", got)
	}
}

func TestScopeCommand_WithoutAdminIDsExplainsItself(t *testing.T) {
	_, f := startQQ(t, map[string]any{"chat_scope": "all"}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 300, "/scope group"))
	f.waitForText(t, "admin_ids", 3*time.Second)
}

func TestScopeCommand_SwitchesAndPersists(t *testing.T) {
	dir := t.TempDir()
	plat, f := startQQ(t, map[string]any{
		"chat_scope":  "all",
		"admin_ids":   "200",
		"cc_data_dir": dir,
		"cc_project":  "proj",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 200, "/scope 私聊"))
	f.waitForText(t, "✅", 3*time.Second)

	if got := plat.currentScope(); got != chatScopePrivate {
		t.Errorf("currentScope = %q, want %q", got, chatScopePrivate)
	}
	if got := loadChatScope(chatScopeStatePath(map[string]any{"cc_data_dir": dir, "cc_project": "proj"})); got != chatScopePrivate {
		t.Errorf("persisted scope = %q, want %q", got, chatScopePrivate)
	}
}

func TestScopeCommand_ShowsCurrentOnNoArgs(t *testing.T) {
	_, f := startQQ(t, map[string]any{
		"chat_scope": "group",
		"admin_ids":  "200",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 200, "/scope"))
	f.waitForText(t, "仅群聊", 3*time.Second)
}

func TestScopeCommand_RejectsInvalidArg(t *testing.T) {
	plat, f := startQQ(t, map[string]any{
		"chat_scope": "all",
		"admin_ids":  "200",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 200, "/scope nonsense"))
	f.waitForText(t, "参数无效", 3*time.Second)

	if got := plat.currentScope(); got != chatScopeAll {
		t.Errorf("currentScope = %q, want unchanged %q", got, chatScopeAll)
	}
}

// TestScopeCommand_WorksFromOutOfScopeChat is the lockout guard: with scope set
// to private, an admin must still be able to switch back from a group.
func TestScopeCommand_WorksFromOutOfScopeChat(t *testing.T) {
	plat, f := startQQ(t, map[string]any{
		"chat_scope": "private",
		"admin_ids":  "200",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 200, "/scope all"))
	f.waitForText(t, "✅", 3*time.Second)

	if got := plat.currentScope(); got != chatScopeAll {
		t.Errorf("currentScope = %q, want %q", got, chatScopeAll)
	}
}

// TestHandlerReplyDoesNotStallReadLoop is a regression test for a self-deadlock.
// A handler issuing an API call can only have its response routed by readLoop, so
// handling events inline on the read loop made the reply wait out callAPI's full
// 15s timeout — and the socket was deaf for those 15 seconds. Two commands sent
// back to back must both be answered promptly; the second reply is the signal,
// since the first one's send is observable on the wire even while it is stuck.
func TestHandlerReplyDoesNotStallReadLoop(t *testing.T) {
	_, f := startQQ(t, map[string]any{
		"chat_scope": "all",
		"admin_ids":  "200",
	}, func(core.Platform, *core.Message) {})

	f.sendEvent(t, textEvent("group", 200, "/scope private"))
	f.sendEvent(t, textEvent("group", 200, "/scope"))

	if !waitUntil(func() bool { return len(f.sentTexts()) >= 2 }, 3*time.Second) {
		t.Fatalf("expected two replies promptly, got %v", f.sentTexts())
	}
}

func waitUntil(pred func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestIsControlCommand(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"/scope", true},
		{"/scope group", true},
		{"/persona", true},
		{"/persona simp", true},
		{"/new", true}, // core built-in
		{"/help", true},
		{"/tts always", true},
		{"/simp", false}, // looks like a command, is chat
		{"/list", true},
		{"你好", false},
		{"", false},
		{"/scopeish", false},
	} {
		if got := isControlCommand(tc.text); got != tc.want {
			t.Errorf("isControlCommand(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestShouldReply_CooldownQuietsAmbientReplies(t *testing.T) {
	p := &Platform{
		replyProbability: 100, // without a cooldown this always replies
		replySkipPenalty: 1,
		replyColdBoost:   0,
		replyProbMin:     1,
		replyProbMax:     100,
		replyCooldown:    60,
	}

	// First ambient message: no prior reply, so the cooldown cannot apply.
	// The text is deliberately longer than two runes and asks no question, so the
	// content modifiers do not interfere.
	if !p.shouldReply("qq:g:1", "今天天气不错") {
		t.Fatal("first message should not be suppressed by the cooldown")
	}

	// Immediately after replying, ambient messages are suppressed.
	if p.shouldReply("qq:g:1", "再聊一句") {
		t.Error("message within the cooldown window should be suppressed")
	}

	// Backdate the last reply past the cooldown: replies are allowed again.
	p.replyStateMap.Store("qq:g:1", &replyState{lastReplyTime: time.Now().Add(-2 * time.Minute)})
	if !p.shouldReply("qq:g:1", "过了一分钟") {
		t.Error("message after the cooldown window should be allowed")
	}
}

func TestShouldReply_CooldownDisabledByDefault(t *testing.T) {
	p := &Platform{replyProbability: 100, replySkipPenalty: 1, replyProbMin: 1, replyProbMax: 100}
	for i := 0; i < 5; i++ {
		if !p.shouldReply("qq:g:1", "今天天气不错") {
			t.Fatal("replyCooldown 0 should disable the cooldown entirely")
		}
	}
}

// TestAtMentionLeavesReplyStateUntouched pins the contract that being addressed
// directly answers 100% of the time yet neither spends nor resets the ambient
// reply budget, so @ traffic does not change how chatty the bot is otherwise.
func TestAtMentionLeavesReplyStateUntouched(t *testing.T) {
	got := make(chan *core.Message, 4)
	plat, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_cooldown":           3600, // would silence any ambient message
	}, func(_ core.Platform, m *core.Message) { got <- m })

	last := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	plat.replyStateMap.Store("qq:g:100", &replyState{skipCount: 5, lastReplyTime: last})

	f.sendEvent(t, map[string]any{
		"post_type":    "message",
		"message_type": "group",
		"group_id":     int64(100),
		"user_id":      int64(200),
		"message_id":   int64(1),
		"message": []any{
			map[string]any{"type": "at", "data": map[string]any{"qq": strconv.Itoa(fakeBotUserID)}},
			map[string]any{"type": "text", "data": map[string]any{"text": " 在吗"}},
		},
	})

	select {
	case <-got: // @-mention is delivered despite the long cooldown
	case <-time.After(3 * time.Second):
		t.Fatal("@-mention was not delivered; it must not be throttled")
	}

	raw, ok := plat.replyStateMap.Load("qq:g:100")
	if !ok {
		t.Fatal("reply state disappeared")
	}
	rs := raw.(*replyState)
	if rs.skipCount != 5 {
		t.Errorf("skipCount = %d, want 5 untouched by an @-mention", rs.skipCount)
	}
	if !rs.lastReplyTime.Equal(last) {
		t.Errorf("lastReplyTime = %v, want untouched %v", rs.lastReplyTime, last)
	}
}

// TestCommandSurvivesBufferFlush is a regression test: the backlog used to
// replace the current message's text, so the engine (which dispatches commands by
// a leading "/") turned e.g. /new into ordinary chat whenever a backlog existed.
func TestCommandSurvivesBufferFlush(t *testing.T) {
	got := make(chan string, 8)
	plat, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_cooldown":           3600, // keep ambient messages buffered, never replied to
		"reply_probability":        100,
	}, func(_ core.Platform, m *core.Message) { got <- m.Content })

	// Pretend the bot just spoke, so the cooldown suppresses the next two messages
	// and they pile up in the buffer instead of being flushed.
	plat.replyStateMap.Store("qq:g:100", &replyState{lastReplyTime: time.Now()})

	f.sendEvent(t, textEvent("group", 200, "随便聊聊"))
	f.sendEvent(t, textEvent("group", 200, "再聊一句"))
	f.sendEvent(t, textEvent("group", 200, "/new"))

	select {
	case content := <-got:
		if content != "/new" {
			t.Errorf("handler saw %q, want the command %q (backlog must not replace it)", content, "/new")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command never reached the handler")
	}
}

// TestOrdinaryApprovalWordsAreChat is a regression test. A non-admin saying
// "好的" in a group used to be treated as an attempt to approve a tool permission
// request and bounced with "❌ 只有管理员才能批准操作，你一边呆着去。" — even though
// no permission was pending. Deciding that is the engine's job, since only it
// knows whether a request is actually waiting.
func TestOrdinaryApprovalWordsAreChat(t *testing.T) {
	got := make(chan *core.Message, 8)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"tool_admin_only":          true,
		"admin_ids":                "999",
		"reply_probability":        0, // no dice: every message reaches the handler
	}, func(_ core.Platform, m *core.Message) { got <- m })

	for _, text := range []string{"好的", "好", "可以", "ok", "确认", "是"} {
		f.sendEvent(t, textEvent("group", 200, text))
	}

	deadline := time.After(3 * time.Second)
	for i := 0; i < 6; i++ {
		select {
		case <-got:
		case <-deadline:
			t.Fatalf("only %d of 6 approval-like messages reached the handler as chat", i)
		}
	}

	for _, sent := range f.sentTexts() {
		if strings.Contains(sent, "只有管理员才能批准操作") {
			t.Fatalf("a plain chat message was rejected as an unauthorized approval: %q", sent)
		}
	}
}

// TestMessageCarriesPermissionApprovalBlock checks the adapter only labels who may
// not authorize tool use; the engine enforces it while a request is pending.
func TestMessageCarriesPermissionApprovalBlock(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"tool_admin_only":          true,
		"admin_ids":                "2232095290",
		"reply_probability":        0, // reach the handler regardless of the dice
	}, func(_ core.Platform, m *core.Message) { got <- m })

	f.sendEvent(t, textEvent("group", 2232095290, "你好")) // admin
	f.sendEvent(t, textEvent("group", 300000001, "在吗"))  // non-admin

	next := func() *core.Message {
		select {
		case m := <-got:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("message never reached the handler")
			return nil
		}
	}

	if m := next(); m.BlockPermissionApproval {
		t.Error("admin message should be allowed to authorize tool use")
	}
	if m := next(); !m.BlockPermissionApproval {
		t.Error("non-admin message must not be allowed to authorize tool use")
	}
}

// TestAdminCanApproveWithoutToolAdminOnly covers the flag being off entirely.
func TestNoApprovalBlockWhenToolAdminOnlyDisabled(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"tool_admin_only":          false,
		"admin_ids":                "2232095290",
		"reply_probability":        0,
	}, func(_ core.Platform, m *core.Message) { got <- m })

	f.sendEvent(t, textEvent("group", 300000001, "你好"))

	select {
	case m := <-got:
		if m.BlockPermissionApproval {
			t.Error("no approval block should apply when tool_admin_only is off")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message never reached the handler")
	}
}

// TestParseMessage_MarksImages makes sure an image contributes a text marker, the
// way a voice message contributes "[语音 3s]". Without it an image-only group
// message reaches the agent with no text at all.
func TestParseMessage_MarksImages(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "fake")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer ts.Close()

	p := &Platform{}
	payload := map[string]any{
		"message": []any{
			map[string]any{"type": "image", "data": map[string]any{"url": ts.URL}},
		},
	}
	text, images, _, fromVoice := p.parseMessage(payload)

	if text != "[图片]" {
		t.Errorf("text = %q, want %q", text, "[图片]")
	}
	if len(images) != 1 {
		t.Fatalf("got %d images, want 1", len(images))
	}
	if images[0].MimeType != "image/png" || len(images[0].Data) == 0 {
		t.Errorf("bad attachment: mime=%q len=%d", images[0].MimeType, len(images[0].Data))
	}
	if fromVoice {
		t.Error("an image is not a voice message")
	}
}

// TestImageSkipsDice checks that an image does not need to win the reply dice:
// a dropped attachment cannot be recovered, because the backlog keeps text only.
// The dice are pinned to 1% here to make that unambiguous.
func TestImageSkipsDice(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_probability":        1,
		"reply_prob_min":           1,
		"reply_prob_max":           1,
		"reply_cooldown":           0,
	}, func(_ core.Platform, m *core.Message) { got <- m })

	sendTestImage(t, f, 100, 200)

	select {
	case m := <-got:
		if len(m.Images) != 1 {
			t.Fatalf("delivered message carries %d images, want 1", len(m.Images))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the image never reached the handler despite the dice being bypassed")
	}
}

// TestImageRespectsCooldown pins the other half: images are still held to the
// cooldown, so a burst of pictures does not make the bot chatty.
func TestImageRespectsCooldown(t *testing.T) {
	got := make(chan *core.Message, 4)
	plat, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_probability":        100,
		"reply_cooldown":           3600,
	}, func(_ core.Platform, m *core.Message) { got <- m })

	plat.replyStateMap.Store("qq:g:100", &replyState{lastReplyTime: time.Now()})

	sendTestImage(t, f, 100, 200)

	select {
	case m := <-got:
		t.Fatalf("image delivered inside the cooldown window: %+v", m)
	case <-time.After(400 * time.Millisecond):
	}
}

// TestImageReplyRestartsCooldown covers the detail that makes the cooldown bite:
// answering a picture advances the clock too, otherwise the cooldown would only
// ever be driven by dice replies, which are the rare case.
func TestImageReplyRestartsCooldown(t *testing.T) {
	got := make(chan *core.Message, 4)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_probability":        100,
		"reply_cooldown":           3600,
	}, func(_ core.Platform, m *core.Message) { got <- m })

	// No prior reply, so the cooldown cannot apply: this one gets through.
	sendTestImage(t, f, 100, 200)
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the first image never reached the handler")
	}

	// ...and it restarted the cooldown, so the next one is dropped.
	sendTestImage(t, f, 100, 201)
	select {
	case m := <-got:
		t.Fatalf("a second image slipped past the cooldown restarted by the first: %+v", m)
	case <-time.After(400 * time.Millisecond):
	}
}

// sendTestImage pushes an image-only group message whose attachment is fetchable.
func sendTestImage(t *testing.T, f *fakeOneBot, groupID, userID int64) {
	t.Helper()
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	t.Cleanup(img.Close)

	f.sendEvent(t, map[string]any{
		"post_type":    "message",
		"message_type": "group",
		"group_id":     groupID,
		"user_id":      userID,
		"message_id":   time.Now().UnixNano(),
		"message": []any{
			map[string]any{"type": "image", "data": map[string]any{"url": img.URL}},
		},
	})
}

// TestEveryPersonaIsSelectable guards the listing, the aliases and the persona's
// own name all resolving. "捧哏" used to be described in the prompt but missing
// from the lookup table, which made it impossible to select.
func TestEveryPersonaIsSelectable(t *testing.T) {
	for _, c := range personaCommands {
		if got := validPersonas[c.persona]; got != c.persona {
			t.Errorf("persona %q resolves to %q, want itself", c.persona, got)
		}
		for _, alias := range c.aliases {
			if got := validPersonas[alias]; got != c.persona {
				t.Errorf("alias %q resolves to %q, want %q", alias, got, c.persona)
			}
		}
		if !strings.Contains(personaListText(), c.persona) {
			t.Errorf("persona %q is missing from the /persona listing", c.persona)
		}
	}
	if validPersonas["没有这个人设"] != "" {
		t.Error("unknown names must not resolve")
	}
}

// TestPersonaCommandsCoverPrompt keeps the adapter and the agent's prompt in step:
// a persona the adapter can select but the prompt never describes would make the
// agent improvise, and one described but unselectable is simply dead.
func TestPersonaCommandsCoverPrompt(t *testing.T) {
	path := filepath.Join("..", "..", "..", "CLAUDE.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("prompt file not available (%v); run from a full checkout to check the pairing", err)
	}
	re := regexp.MustCompile("(?m)^`\\[([^]]+)\\]`")
	found := re.FindAllStringSubmatch(string(raw), -1)
	if len(found) == 0 {
		t.Fatalf("no persona blocks found in %s", path)
	}
	for _, m := range found {
		tag := m[1]
		if validPersonas[tag] != tag {
			t.Errorf("prompt describes persona %q but /persona cannot select it", tag)
		}
	}
	// ...and the reverse: every selectable persona should be described.
	described := make(map[string]bool, len(found))
	for _, m := range found {
		described[m[1]] = true
	}
	for _, c := range personaCommands {
		if !described[c.persona] {
			t.Errorf("/persona offers %q but the prompt never describes it", c.persona)
		}
	}
}

func TestPersonaSwitchAppliesToLaterMessages(t *testing.T) {
	got := make(chan *core.Message, 8)
	_, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
		"reply_probability":        0,
	}, func(_ core.Platform, m *core.Message) { got <- m })

	f.sendEvent(t, textEvent("group", 200, "/persona 中二病"))
	f.sendEvent(t, textEvent("group", 200, "你好"))

	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-got:
			// The text may have been replaced by the buffered backlog, which also
			// carries the message, so match on the content rather than equality.
			if !strings.Contains(m.Content, "你好") {
				continue // the auto /new that the switch triggers
			}
			if !strings.Contains(m.ExtraContent, "[中二病]") {
				t.Fatalf("ExtraContent = %q, want it to carry the switched persona", m.ExtraContent)
			}
			return
		case <-deadline:
			t.Fatal("the message after /persona never reached the handler")
		}
	}
}

// TestValidateImageRejectsErrorPayload is the regression test for images that were
// silently replaced by QQ's error body. NapCat hands back HTTP 200 with
// {"retcode":-5503007,"retmsg":"download url has expired"}, which used to be saved
// as an "image" and sent to the agent — so the bot truthfully reported that the
// picture was broken.
func TestValidateImageRejectsErrorPayload(t *testing.T) {
	expired := []byte(`{"retcode":-5503007,"retmsg":"download url has expired","retryflag":0}`)
	if _, ok := validateImage(expired); ok {
		t.Fatal("an expired-url error payload must not be accepted as an image")
	}
	if _, ok := validateImage(nil); ok {
		t.Fatal("empty data must not be accepted")
	}
	if _, ok := validateImage([]byte("<html>403</html>")); ok {
		t.Fatal("html must not be accepted")
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	img, ok := validateImage(png)
	if !ok {
		t.Fatal("a real PNG must be accepted")
	}
	if img.MimeType != "image/png" {
		t.Errorf("MimeType = %q, want image/png (sniffed from the bytes)", img.MimeType)
	}
}

func TestParseMessage_MarksFailedImageFetch(t *testing.T) {
	// No url and no file id: the fetch cannot succeed and must say so rather than
	// leaving a bare "[图片]" that invites the agent to invent an excuse.
	p := &Platform{}
	payload := map[string]any{
		"message": []any{
			map[string]any{"type": "image", "data": map[string]any{"file": "x.jpg"}},
		},
	}
	text, images, _, _ := p.parseMessage(payload)
	if text != "[图片(加载失败)]" {
		t.Errorf("text = %q, want %q", text, "[图片(加载失败)]")
	}
	if len(images) != 0 {
		t.Errorf("got %d images, want 0", len(images))
	}
}

func TestChatKind(t *testing.T) {
	if got := chatKind(map[string]any{"message_type": "group"}); got != "group" {
		t.Errorf("chatKind = %q, want group", got)
	}
	if got := chatKind(map[string]any{"message_type": "private"}); got != "private" {
		t.Errorf("chatKind = %q, want private", got)
	}
	if got := chatKind(map[string]any{}); got != "private" {
		t.Errorf("chatKind = %q, want private (default)", got)
	}
}

// TestWithFreshRKeyRebuildsURL pins the fix for images arriving as
// {"retcode":-5503007,"retmsg":"download url has expired"}: the event url keeps
// its appid/fileid but the stale rkey is replaced with a current one.
func TestWithFreshRKeyRebuildsURL(t *testing.T) {
	p := &Platform{
		rkeyMap: map[string]rkeyEntry{
			"group": {value: "FRESHGROUPKEY", fetchedAt: time.Now()},
		},
	}
	stale := "https://multimedia.nt.qq.com.cn/download?appid=1407&fileid=ABC123&rkey=STALEKEY"

	got := p.withFreshRKey(stale, "group")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("rebuilt url does not parse: %v", err)
	}
	q := u.Query()
	if q.Get("rkey") != "FRESHGROUPKEY" {
		t.Errorf("rkey = %q, want the fresh one", q.Get("rkey"))
	}
	if q.Get("fileid") != "ABC123" || q.Get("appid") != "1407" {
		t.Errorf("appid/fileid were lost: %v", u.RawQuery)
	}
	if u.Host != "multimedia.nt.qq.com.cn" {
		t.Errorf("host = %q", u.Host)
	}
}

func TestWithFreshRKeySkipsNonCDNURL(t *testing.T) {
	p := &Platform{rkeyMap: map[string]rkeyEntry{"group": {value: "K", fetchedAt: time.Now()}}}
	for _, in := range []string{"", "not a url", "http://example.com/plain.jpg", "https://x.com/d?appid=1"} {
		if got := p.withFreshRKey(in, "group"); got != "" {
			t.Errorf("withFreshRKey(%q) = %q, want empty", in, got)
		}
	}
}

func TestCronJobs_AcceptLiteralMessages(t *testing.T) {
	p, err := New(map[string]any{
		"cron_jobs": []any{
			map[string]any{"cron": "0 9 * * *", "message": "/签到", "at": int64(3889045760), "group_id": int64(341353242)},
			map[string]any{"cron": "0 10 * * *", "prompt": "说点什么", "group_id": int64(341353242)},
			map[string]any{"cron": "0 11 * * *", "group_id": int64(341353242)}, // neither → ignored
			map[string]any{"cron": "0 12 * * *", "message": "/x"},              // no group → ignored
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	jobs := p.(*Platform).cronJobs
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 (only entries with a prompt or message)", len(jobs))
	}
	if jobs[0].Message != "/签到" || jobs[0].At != 3889045760 {
		t.Errorf("literal job parsed wrong: %+v", jobs[0])
	}
	if jobs[1].Prompt != "说点什么" {
		t.Errorf("prompt job parsed wrong: %+v", jobs[1])
	}
}

// TestExecuteCronMessageSendsMentionAndText covers the shape another bot has to
// receive: a real at-segment followed by the command, sent verbatim, without the
// agent being consulted.
func TestExecuteCronMessageSendsMentionAndText(t *testing.T) {
	handled := make(chan *core.Message, 4)
	plat, f := startQQ(t, map[string]any{
		"share_session_in_channel": true,
	}, func(_ core.Platform, m *core.Message) { handled <- m })

	plat.executeCronMessage(cronJobConfig{
		Cron: "0 9 * * *", Message: "/签到", At: 3889045760, GroupID: 341353242,
	})

	params := f.sentParams()
	if len(params) != 1 {
		t.Fatalf("got %d outgoing messages, want 1", len(params))
	}
	if got := params[0]["group_id"]; got != int64(341353242) && got != float64(341353242) {
		t.Errorf("group_id = %v", got)
	}
	segs, ok := params[0]["message"].([]any)
	if !ok {
		t.Fatalf("message = %T, want a segment array: %v", params[0]["message"], params[0]["message"])
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want at + text: %v", len(segs), segs)
	}
	first := segs[0].(map[string]any)
	if first["type"] != "at" || first["data"].(map[string]any)["qq"] != "3889045760" {
		t.Errorf("first segment should be the mention: %v", first)
	}
	second := segs[1].(map[string]any)
	if second["type"] != "text" || second["data"].(map[string]any)["text"] != "/签到" {
		t.Errorf("second segment should be the command: %v", second)
	}

	select {
	case m := <-handled:
		t.Errorf("a literal cron message must not reach the agent, got %q", m.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestExecuteCronMessageRespectsScope(t *testing.T) {
	plat, f := startQQ(t, map[string]any{"chat_scope": "private"}, func(core.Platform, *core.Message) {})
	plat.executeCronMessage(cronJobConfig{Message: "/签到", GroupID: 341353242})
	if n := len(f.sentParams()); n != 0 {
		t.Errorf("groups are out of scope but %d message(s) went out", n)
	}
}
