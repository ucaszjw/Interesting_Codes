package qq

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
	"github.com/robfig/cron/v3"
)

func init() {
	core.RegisterPlatform("qq", New)
}

// Platform connects to a OneBot v11 implementation (NapCat, LLOneBot, etc.)
// via forward WebSocket. It receives message events and sends messages back
// through the same WS connection.
type Platform struct {
	wsURL                 string // e.g. "ws://127.0.0.1:3001"
	token                 string // optional access_token
	allowFrom             string // comma-separated user IDs or "*"
	shareSessionInChannel bool
	requireAt             bool   // only respond to group messages that @ the bot
	toolAdminOnly         bool   // non-admin users get plan mode (chat only, no tools)
	adminIDs              string // comma-separated admin user IDs
	groupAllow            string // comma-separated group IDs or "*"
	handler               core.MessageHandler
	conn                  *websocket.Conn
	mu                    sync.Mutex
	echoSeq               atomic.Int64
	echoCh                sync.Map // echo -> chan json.RawMessage
	cancel                context.CancelFunc
	selfID                int64
	dedup                 core.MessageDedup
	groupNameCache        sync.Map // groupID -> group name

	// Group-only feature flags (all default false)
	replyWithQuote bool   // send replies with quote of original message
	emojiLike      bool   // add emoji reaction acknowledging received messages
	emojiLikeDef   string // default emoji_id for reaction; "" = "76" (👍)
	recallComment  bool   // comment when someone recalls a message in group
	checkMute      bool   // skip sending if bot is muted in the group
	whisperEnabled bool   // enable local Whisper ASR for voice messages
	whisperModel   string // path to whisper model file (.bin)

	// TTS: synthesis and gating live in core (see core/tts.go). The platform only
	// picks the voice per persona and may thin out voice replies by probability.
	voiceProbability int               // % chance an eligible reply is spoken; 0 = always (no gating)
	personaVoices    map[string]string // persona tag -> voice name understood by the configured provider

	// Chat scope: which chat types the bot answers in. Switched at runtime by the
	// admin-only /scope command and persisted under the data dir.
	scopeMu   sync.Mutex
	chatScope string // chatScopeAll | chatScopeGroup | chatScopePrivate
	scopePath string // "" when the data dir / project name were not injected

	replyProbability int // base reply probability (%, 0 = disabled)
	replySkipPenalty int // probability decrease per skipped message
	replyColdBoost   int // probability increase per 10s of silence
	replyProbMin     int // minimum probability (%)
	replyProbMax     int // maximum probability (%)
	// replyCooldown is the minimum number of seconds between replies to messages
	// that did not address the bot. An @-mention always gets an answer and neither
	// honours nor resets this, so a direct question is never ignored.
	replyCooldown int

	// Scheduled tasks (cron): the prompt is sent to the AI for dynamic generation
	cronJobs      []cronJobConfig
	cronScheduler *cron.Cron // nil if no cron jobs

	// Mute cache: groupID -> *muteCacheEntry
	muteCache   sync.Map
	muteCacheMu sync.Mutex

	// Recent message cache for recall content lookup (messageID -> info)
	recentMessages   map[int64]*recallEntry
	recentMessagesMu sync.Mutex

	// Tracks which messages have received emoji reactions (avoids duplicates)
	emojiReacted sync.Map // messageID string -> bool

	// Persona mapping: sessionKey -> persona tag name
	personaMap sync.Map

	// Reply probability state per sessionKey
	replyStateMap sync.Map

	// Message buffer per sessionKey (accumulates group messages between AI replies)
	msgBufferMap sync.Map

	// Events handed from readLoop to dispatchLoop. Handling happens off the read
	// loop so API calls issued from a handler (the /scope and /persona replies)
	// can still have their responses routed, which readLoop is responsible for.
	msgQueue chan map[string]any

	// QQ CDN rkeys cached per chat kind ("group"/"private"). The rkey inside a
	// message event's image url can already be stale, so downloads are rebuilt
	// with a fresh one from get_rkey.
	rkeyMu  sync.Mutex
	rkeyMap map[string]rkeyEntry
}

// rkeyEntry is a cached CDN rkey with the time it was fetched.
type rkeyEntry struct {
	value     string
	fetchedAt time.Time
}

// rkeyRefreshInterval is how long a cached rkey is reused. QQ hands out rkeys with
// a ttl of around an hour; refreshing well inside that keeps a margin.
const rkeyRefreshInterval = 20 * time.Minute

// msgQueueSize bounds how many events may wait behind a slow handler.
const msgQueueSize = 64

// Chat scope values for the chat_scope option and the /scope command.
const (
	chatScopeAll     = "all"     // answer in both private chats and groups
	chatScopeGroup   = "group"   // answer in groups only
	chatScopePrivate = "private" // answer in private chats only
)

// personaCommands is the single source of truth for /persona: each persona tag
// with the aliases that select it, in the order shown to users. Personas must also
// be described in the prompt file (CLAUDE.md) for the agent to know how to play
// them — TestPersonaCommandsCoverPrompt guards that the two stay in step.
var personaCommands = []struct {
	persona string
	aliases []string
}{
	{"贴吧老哥", []string{"tieba", "老哥"}},
	{"猫娘", []string{"neko", "catgirl"}},
	{"老干部", []string{"cadre", "老干"}},
	{"母狗", []string{"simp"}},
	{"萌妹", []string{"cute"}},
	{"捧哏", []string{"penggen"}},
	{"直男", []string{"straight"}},
	{"魅魔", []string{"succubus"}},
	{"女拳", []string{"feminist"}},
	{"赛博道士", []string{"taoist", "道士"}},
	{"弱智吧吧友", []string{"ruozhi", "弱智"}},
	{"资本家", []string{"capitalist"}},
	{"营销号", []string{"marketing"}},
	{"复读机", []string{"repeater"}},
	{"文豪", []string{"poet"}},
	{"理中客", []string{"reasonable"}},
	{"甲方", []string{"client"}},
	{"中二病", []string{"chuuni", "中二"}},
	{"男朋友", []string{"bf", "boyfriend"}},
	{"女朋友", []string{"gf", "girlfriend"}},
	{"群友", []string{"qunyou", "群u"}},
}

// validPersonas resolves a /persona argument (or a persona tag itself) to its tag.
var validPersonas = func() map[string]string {
	m := make(map[string]string, 2*len(personaCommands))
	for _, c := range personaCommands {
		m[c.persona] = c.persona
		for _, a := range c.aliases {
			m[a] = c.persona
		}
	}
	return m
}()

// personaListText renders the available personas for the /persona help reply.
func personaListText() string {
	parts := make([]string, 0, len(personaCommands))
	for _, c := range personaCommands {
		parts = append(parts, c.persona+"/"+strings.Join(c.aliases, "/"))
	}
	return strings.Join(parts, ", ")
}

// cronJobConfig defines a scheduled group action. A job either asks the agent to
// compose something (prompt) or sends fixed text verbatim (message).
type cronJobConfig struct {
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Message string `json:"message"` // literal text, sent without the agent
	At      int64  `json:"at"`      // optional QQ to @-mention before Message
	GroupID int64  `json:"group_id"`
}

// recallEntry stores info about a recent message for recall detection.
type recallEntry struct {
	userID    int64
	content   string
	timestamp time.Time
}

// muteCacheEntry caches the bot's mute status in a group.
type muteCacheEntry struct {
	isMuted  bool
	until    time.Time // when the mute ends (if isMuted)
	cachedAt time.Time
}

// replyState tracks probability-based reply state per session.
type replyState struct {
	skipCount     int       // consecutive skipped messages
	lastReplyTime time.Time // last time we replied
}

// msgBuffer accumulates group messages between AI replies for full context.
type msgBuffer struct {
	lines []string  // each: "sender_name(qq): text"
	start time.Time // when first message was added
}

func New(opts map[string]any) (core.Platform, error) {
	wsURL, _ := opts["ws_url"].(string)
	if wsURL == "" {
		wsURL = "ws://127.0.0.1:3001"
	}
	token, _ := opts["token"].(string)
	allowFrom, _ := opts["allow_from"].(string)
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	requireAt, _ := opts["require_at"].(bool)
	toolAdminOnly, _ := opts["tool_admin_only"].(bool)
	adminIDs, _ := opts["admin_ids"].(string)
	groupAllow, _ := opts["group_allow"].(string)

	// Group-only features
	replyWithQuote, _ := opts["reply_with_quote"].(bool)
	emojiLike, _ := opts["emoji_like"].(bool)
	recallComment, _ := opts["recall_comment"].(bool)
	checkMute, _ := opts["check_mute"].(bool)
	whisperEnabled, _ := opts["whisper_enabled"].(bool)
	whisperModel, _ := opts["whisper_model"].(string)
	voiceProbability := optInt(opts, "voice_probability")
	replyProbability := optIntOr(opts, "reply_probability", 30)
	replySkipPenalty := optIntOr(opts, "reply_skip_penalty", 1)
	replyColdBoost := optIntOr(opts, "reply_cold_boost", 5)
	replyProbMin := optIntOr(opts, "reply_prob_min", 10)
	replyProbMax := optIntOr(opts, "reply_prob_max", 50)
	replyCooldown := optInt(opts, "reply_cooldown")
	emojiLikeDef, _ := opts["emoji_like_id"].(string)

	personaVoices := map[string]string{}
	if m, ok := opts["persona_voices"].(map[string]any); ok {
		for persona, v := range m {
			if voice, ok := v.(string); ok && voice != "" {
				personaVoices[persona] = voice
			}
		}
	}

	// Chat scope: the config value is the default, a previously persisted /scope
	// switch wins because it records the most recent explicit intent.
	chatScope := parseChatScope(toString(opts["chat_scope"]))
	if chatScope == "" {
		chatScope = chatScopeAll
	}
	scopePath := chatScopeStatePath(opts)
	if saved := loadChatScope(scopePath); saved != "" {
		chatScope = saved
	}

	if voiceProbability < 0 {
		voiceProbability = 0
	}
	if voiceProbability > 100 {
		voiceProbability = 100
	}

	// Cron jobs: prompts sent to the AI for dynamic generation
	var cronJobs []cronJobConfig
	if raw, ok := opts["cron_jobs"].([]any); ok {
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				cron := toString(m["cron"])
				prompt := toString(m["prompt"])
				message := toString(m["message"])
				at, _ := toInt64(m["at"])
				gid, _ := toInt64(m["group_id"])
				if cron == "" || gid == 0 || (prompt == "" && message == "") {
					continue
				}
				cronJobs = append(cronJobs, cronJobConfig{
					Cron: cron, Prompt: prompt, Message: message, At: at, GroupID: gid,
				})
			}
		}
	}

	core.CheckAllowFrom("qq", allowFrom)
	return &Platform{
		wsURL:                 wsURL,
		token:                 token,
		allowFrom:             allowFrom,
		shareSessionInChannel: shareSessionInChannel,
		requireAt:             requireAt,
		toolAdminOnly:         toolAdminOnly,
		adminIDs:              adminIDs,
		groupAllow:            groupAllow,
		replyWithQuote:        replyWithQuote,
		emojiLike:             emojiLike,
		recallComment:         recallComment,
		checkMute:             checkMute,
		whisperEnabled:        whisperEnabled,
		whisperModel:          whisperModel,
		voiceProbability:      voiceProbability,
		personaVoices:         personaVoices,
		chatScope:             chatScope,
		scopePath:             scopePath,
		replyProbability:      replyProbability,
		replySkipPenalty:      replySkipPenalty,
		replyColdBoost:        replyColdBoost,
		replyProbMin:          replyProbMin,
		replyProbMax:          replyProbMax,
		replyCooldown:         replyCooldown,
		emojiLikeDef:          emojiLikeDef,
		cronJobs:              cronJobs,
		recentMessages:        make(map[int64]*recallEntry),
	}, nil
}

func (p *Platform) Name() string { return "qq" }

func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler

	header := http.Header{}
	if p.token != "" {
		header.Set("Authorization", "Bearer "+p.token)
	}

	conn, _, err := websocket.DefaultDialer.Dial(p.wsURL, header)
	if err != nil {
		return fmt.Errorf("qq: ws connect failed (%s): %w", p.wsURL, err)
	}
	p.conn = conn

	slog.Info("qq: connected to OneBot", "url", p.wsURL)

	if p.replyProbability > 0 {
		slog.Info("qq: reply throttle",
			"at_mention", "always",
			"probability", p.replyProbability,
			"prob_range", fmt.Sprintf("%d-%d", p.replyProbMin, p.replyProbMax),
			"cold_boost_per_10s", p.replyColdBoost,
			"skip_penalty", p.replySkipPenalty,
			"cooldown_s", p.replyCooldown)
	} else {
		slog.Warn("qq: probability gating disabled, every group message reaches the agent")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	if p.msgQueue == nil {
		p.msgQueue = make(chan map[string]any, msgQueueSize)
	}
	go p.dispatchLoop(ctx)

	// Start readLoop BEFORE callAPI: callAPI's response is routed by readLoop,
	// so calling it first would always time out after 15s and leave selfID=0,
	// which disables the self-message filter in handleMessage and lets the bot
	// respond to its own messages.
	go p.readLoop(ctx)

	// Get bot self info
	if info, err := p.callAPI("get_login_info", nil); err == nil {
		if uid, ok := info["user_id"].(float64); ok {
			p.selfID = int64(uid)
		}
		nick, _ := info["nickname"].(string)
		slog.Info("qq: logged in", "qq", p.selfID, "nickname", nick)
	} else {
		slog.Warn("qq: get_login_info failed; self-message filter disabled until next reconnect", "error", err)
	}

	// Start scheduled messages
	p.startCronJobs()

	return nil
}

func (p *Platform) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, raw, err := p.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("qq: ws read error, reconnecting...", "error", err)
			p.reconnect()
			continue
		}

		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}

		// If this is an API response (has "echo" field), route to caller
		if echo, ok := payload["echo"].(string); ok {
			if ch, loaded := p.echoCh.LoadAndDelete(echo); loaded {
				if dataCh, ok := ch.(chan json.RawMessage); ok {
					dataCh <- raw
				}
			}
			continue
		}

		// Otherwise it's an event; hand it to the dispatcher so this loop stays
		// free to route API responses and answer pings.
		select {
		case p.msgQueue <- payload:
		case <-ctx.Done():
			return
		}
	}
}

// dispatchLoop runs message and notice handling on its own goroutine. Handlers
// issue API calls (incoming-message replies, emoji reactions, mute checks) whose
// responses can only be routed by readLoop, so handling events inline would
// deadlock until callAPI's 15s timeout. Running them here keeps readLoop free
// while still serializing handlers against each other.
func (p *Platform) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-p.msgQueue:
			switch ev["post_type"] {
			case "message":
				p.handleMessage(ev)
			case "notice":
				p.handleNotice(ev)
			}
		}
	}
}

func (p *Platform) reconnect() {
	for i := 1; i <= 30; i++ {
		time.Sleep(time.Duration(i) * 2 * time.Second)
		header := http.Header{}
		if p.token != "" {
			header.Set("Authorization", "Bearer "+p.token)
		}
		conn, _, err := websocket.DefaultDialer.Dial(p.wsURL, header)
		if err != nil {
			slog.Warn("qq: reconnect attempt failed", "attempt", i, "error", err)
			continue
		}
		p.mu.Lock()
		p.conn = conn
		p.mu.Unlock()
		slog.Info("qq: reconnected")
		return
	}
	slog.Error("qq: failed to reconnect after 30 attempts")
}

func (p *Platform) handleMessage(payload map[string]any) {
	msgType, _ := payload["message_type"].(string)
	userID := jsonInt64(payload, "user_id")
	groupID := jsonInt64(payload, "group_id")
	messageID := jsonInt64(payload, "message_id")

	if userID == p.selfID {
		return
	}

	if ts, ok := payload["time"].(float64); ok && ts > 0 {
		if core.IsOldMessage(time.Unix(int64(ts), 0)) {
			slog.Debug("qq: ignoring old message after restart", "time", int64(ts))
			return
		}
	}

	msgIDStr := strconv.FormatInt(messageID, 10)
	if p.dedup.IsDuplicate(msgIDStr) {
		slog.Debug("qq: duplicate message ignored", "message_id", messageID)
		return
	}

	if !p.isAllowed(userID) {
		return
	}

	// Group allowlist check
	if groupID != 0 && !p.isGroupAllowed(groupID) {
		return
	}

	// Chat scope: drop chats the bot is not answering in. Text is read without
	// downloading attachments so ignored chats cost nothing.
	if p.scopeBlocks(msgType, rawTextOf(payload)) {
		slog.Debug("qq: ignoring message outside chat scope", "scope", p.currentScope(), "type", msgType)
		return
	}

	// If require_at is set, only respond to group messages that @ the bot
	if p.requireAt && msgType == "group" && !p.isBotMentioned(payload) {
		return
	}

	// Extract sender info
	var userName string
	if sender, ok := payload["sender"].(map[string]any); ok {
		card, _ := sender["card"].(string)
		nick, _ := sender["nickname"].(string)
		if card != "" {
			userName = card
		} else {
			userName = nick
		}
	}

	// Parse message content from CQ message array or raw_message
	text, images, audio, fromVoice := p.parseMessage(payload)
	if text == "" && len(images) == 0 && audio == nil {
		return
	}

	var sessionKey string
	if msgType == "group" {
		if p.shareSessionInChannel {
			sessionKey = fmt.Sprintf("qq:g:%d", groupID)
		} else {
			sessionKey = fmt.Sprintf("qq:%d:%d", groupID, userID)
		}
	} else {
		sessionKey = fmt.Sprintf("qq:%d", userID)
	}

	// Control messages are answered unconditionally and are never throttled, so
	// admin workflows stay responsive. Only recognized commands count: treating
	// any "/" prefix as a command let "/anything" bypass the throttle.
	control := isControlCommand(text)

	// Buffer: accumulate group messages for full context between AI replies.
	// Commands are left out — the engine acts on them directly, so replaying
	// "/new" or "/persona x" as conversation context is only noise.
	if msgType == "group" && !control {
		p.addToBuffer(sessionKey, userName, userID, text)
	}

	rctx := &replyContext{
		messageType: msgType,
		userID:      userID,
		groupID:     groupID,
		messageID:   int32(messageID),
	}

	// Handle /scope (admin-only) before anything group-specific so switching back
	// from "only private" is possible from a group and vice versa.
	if isCommand(text, "/scope") {
		p.handleScopeCommand(rctx, userID, text)
		return
	}

	var chatName string
	if msgType == "group" {
		chatName = p.resolveGroupName(groupID)
		rctx.persona = p.personaFor(sessionKey)
	}

	// Set ExtraContent to indicate chat type so the agent can differentiate behavior
	var extraContent string
	shortReply := false

	if msgType == "group" || (p.toolAdminOnly && !p.isAdmin(userID)) {
		extraContent = "[群聊消息]"
		// Add persona tag after [群聊消息] (before sender info)
		if msgType == "group" {
			extraContent = p.makeGroupExtraContent(sessionKey)
			if userName != "" {
				extraContent = fmt.Sprintf("%s %s(%d):", extraContent, userName, userID)
			} else {
				extraContent = fmt.Sprintf("%s (%d):", extraContent, userID)
			}
		}
	}

	// If tool_admin_only is set, non-admin users get plan mode (chat only, no tools)
	var modeOverride string
	if p.toolAdminOnly && !p.isAdmin(userID) {
		modeOverride = "plan"
	}

	// Permission replies are the engine's call: only it knows whether a request is
	// actually pending. All this adapter does is say who is not allowed to
	// authorize tool use, and the engine enforces that at the point of decision —
	// so a plain "好的" in chat is treated as chat, not as a refused approval.

	// Handle /persona command (persona switching)
	if msgType == "group" && isCommand(text, "/persona") {
		name := strings.TrimSpace(text[len("/persona"):])
		if name == "" || validPersonas[name] == "" {
			p.Reply(context.Background(), rctx, "可用人设: "+personaListText())
			return
		}
		mapped := validPersonas[name]
		p.personaMap.Store(sessionKey, mapped)
		// Auto /new to reset session with new persona
		newMsg := &core.Message{
			SessionKey:   sessionKey,
			Platform:     "qq",
			MessageID:    fmt.Sprintf("persona_%d", time.Now().UnixMilli()),
			UserID:       strconv.FormatInt(userID, 10),
			UserName:     userName,
			Content:      "/new",
			ExtraContent: extraContent,
			ReplyCtx:     rctx,
		}
		p.handler(p, newMsg)
		return
	}
	// Throttling for group messages that did not address the bot. Being
	// @-mentioned answers 100% of the time and leaves this state untouched, so a
	// direct question neither spends nor resets the ambient reply budget.
	if msgType == "group" && !control && !p.isBotMentioned(payload) && p.replyProbability > 0 {
		if len(images) > 0 || audio != nil {
			// Attachments never roll the dice: a dropped picture or recording is
			// gone for good, since the backlog holds text only. They do observe the
			// cooldown, and answering one restarts it — otherwise the cooldown would
			// barely bite here, because its clock is otherwise only advanced by dice
			// replies, which are the rare case.
			rs := p.replyStateFor(sessionKey)
			now := time.Now()
			if p.inCooldown(rs, now) {
				rs.skipCount++
				slog.Info("qq: skip, cooldown", "session", sessionKey, "kind", "attachment")
				return
			}
			rs.lastReplyTime = now
		} else if !p.shouldReply(sessionKey, text) {
			// shouldReply already logged whether it was the cooldown or the dice.
			slog.Info("qq: skip, throttle", "session", sessionKey, "text", truncateText(text, 20))
			return
		} else {
			shortReply = true
		}
	}

	// When replying, flush buffered messages as context. Commands keep their own
	// text: the engine dispatches them by looking for a leading "/", so replacing
	// it with the backlog would silently turn e.g. /new into ordinary chat.
	if msgType == "group" && !control {
		if buffered := p.flushBuffer(sessionKey); buffered != "" {
			text = buffered
		}
	}

	// 概率触发时简短回复
	if shortReply {
		extraContent += " 简短回复，像日常聊天一样说一两句即可，不要长篇大论"
	}
	msg := &core.Message{
		SessionKey:   sessionKey,
		Platform:     "qq",
		MessageID:    strconv.FormatInt(messageID, 10),
		UserID:       strconv.FormatInt(userID, 10),
		UserName:     userName,
		ChatName:     chatName,
		Content:      text,
		ExtraContent: extraContent,
		Images:       images,
		Audio:        audio,
		FromVoice:    fromVoice,
		ModeOverride: modeOverride,
		// With tool_admin_only, non-admins may chat but must not authorize tool
		// use. The engine only acts on this while a permission is pending.
		BlockPermissionApproval: p.toolAdminOnly && !p.isAdmin(userID),
		ReplyCtx:                rctx,
	}

	// Track recent messages for recall content lookup (group only)
	if p.recallComment && msgType == "group" && messageID != 0 {
		p.trackMessage(messageID, userID, text)
	}

	slog.Debug("qq: message received", "type", msgType, "user", userID, "text_len", len(text))
	p.handler(p, msg)
}

// handleNotice dispatches OneBot notice events.
func (p *Platform) handleNotice(payload map[string]any) {
	noticeType, _ := payload["notice_type"].(string)
	subType, _ := payload["sub_type"].(string)

	switch {
	case noticeType == "notify" && subType == "poke":
		p.handlePoke(payload)
	case noticeType == "group_recall":
		p.handleGroupRecall(payload)
	default:
		// Ignore other notice types
	}
}

// handlePoke handles "戳一戳" (poke) events. Only responds when the bot itself is poked.
func (p *Platform) handlePoke(payload map[string]any) {
	userID := jsonInt64(payload, "user_id")
	targetID := jsonInt64(payload, "target_id")
	groupID := jsonInt64(payload, "group_id")

	// Only respond when the bot is poked
	if targetID != p.selfID {
		return
	}

	if userID == p.selfID {
		return
	}

	if !p.isAllowed(userID) {
		return
	}

	// Group allowlist check
	if groupID != 0 && !p.isGroupAllowed(groupID) {
		return
	}

	if ts, ok := payload["time"].(float64); ok && ts > 0 {
		if core.IsOldMessage(time.Unix(int64(ts), 0)) {
			slog.Debug("qq: ignoring old poke event", "time", int64(ts))
			return
		}
	}

	msgType := "private"
	if groupID != 0 {
		msgType = "group"
	}

	if !p.scopeAllows(msgType) {
		return
	}

	var sessionKey string
	if msgType == "group" {
		if p.shareSessionInChannel {
			sessionKey = fmt.Sprintf("qq:g:%d", groupID)
		} else {
			sessionKey = fmt.Sprintf("qq:%d:%d", groupID, userID)
		}
	} else {
		sessionKey = fmt.Sprintf("qq:%d", userID)
	}

	rctx := &replyContext{
		messageType: msgType,
		userID:      userID,
		groupID:     groupID,
	}

	var chatName string
	if msgType == "group" {
		chatName = p.resolveGroupName(groupID)
		rctx.persona = p.personaFor(sessionKey)
	}

	var extraContent string
	if msgType == "group" || (p.toolAdminOnly && !p.isAdmin(userID)) {
		extraContent = "[群聊消息]"
	}

	var modeOverride string
	if p.toolAdminOnly && !p.isAdmin(userID) {
		modeOverride = "plan"
	}

	msg := &core.Message{
		SessionKey:   sessionKey,
		Platform:     "qq",
		MessageID:    fmt.Sprintf("poke_%d_%d", userID, time.Now().UnixMilli()),
		UserID:       strconv.FormatInt(userID, 10),
		UserName:     strconv.FormatInt(userID, 10),
		ChatName:     chatName,
		Content:      "poke",
		ExtraContent: extraContent,
		ModeOverride: modeOverride,
		ReplyCtx:     rctx,
	}

	slog.Debug("qq: poke received", "type", msgType, "user", userID)
	p.handler(p, msg)
}

func (p *Platform) parseMessage(payload map[string]any) (string, []core.ImageAttachment, *core.AudioAttachment, bool) {
	var textParts []string
	var images []core.ImageAttachment
	var audio *core.AudioAttachment
	fromVoice := false

	// OneBot message can be array of segments or a string
	switch msg := payload["message"].(type) {
	case []any:
		for _, seg := range msg {
			s, ok := seg.(map[string]any)
			if !ok {
				continue
			}
			segType, _ := s["type"].(string)
			data, _ := s["data"].(map[string]any)
			if data == nil {
				continue
			}

			switch segType {
			case "text":
				if text, ok := data["text"].(string); ok {
					textParts = append(textParts, text)
				}
			case "image":
				// Mark the position so the prompt says an image was attached, the
				// same way voice messages contribute "[语音 3s]". Without it an
				// image-only message has no text at all and the agent is left
				// guessing why it received an attachment.
				textParts = append(textParts, "[图片]")
				img, ok := p.fetchImage(data, chatKind(payload))
				if !ok {
					// Say so explicitly rather than leaving a bare "[图片]" next to a
					// missing attachment, which invites the agent to invent an excuse.
					textParts[len(textParts)-1] = "[图片(加载失败)]"
					continue
				}
				images = append(images, img)
			case "record":
				fromVoice = true
				var keys []string
				for key := range data {
					keys = append(keys, key)
				}
				slog.Info("qq: voice data", "keys", keys, "file", data["file"], "path", data["path"])
				// Check for QQ ASR text (voice-to-text transcription)
				if t, ok := data["text"].(string); ok && t != "" {
					textParts = append(textParts, "[语音转文字: "+t+"]")
				} else if file, ok := data["file"].(string); ok {
					// Extract duration from filename if available, e.g. "flag_49s.amr"
					duration := ""
					if idx := strings.LastIndex(file, "_"); idx >= 0 {
						if end := strings.Index(file[idx:], "s"); end > 1 {
							duration = file[idx+1 : idx+end]
						}
					}
					if duration != "" {
						textParts = append(textParts, "[语音 "+duration+"s]")
					} else {
						textParts = append(textParts, "[语音消息]")
					}
				} else {
					textParts = append(textParts, "[语音消息]")
				}
				// Voice-to-text via local Whisper
				if p.whisperEnabled && p.whisperModel != "" {
					slog.Info("qq: whisper processing", "has_url", data["url"] != nil)
					if url, ok := data["url"].(string); ok && url != "" {
						if transcript := p.transcribeAudio(url); transcript != "" {
							textParts[len(textParts)-1] = "[语音: " + transcript + "]"
						}
					}
				}
				// Also download audio for agents that support it
				if url, ok := data["url"].(string); ok && url != "" {
					audioData, _, err := downloadFile(url)
					if err != nil {
						slog.Warn("qq: download audio failed", "error", err)
						continue
					}
					format := "silk"
					if f, ok := data["file"].(string); ok {
						if strings.HasSuffix(f, ".amr") {
							format = "amr"
						} else if strings.HasSuffix(f, ".mp3") {
							format = "mp3"
						}
					}
					audio = &core.AudioAttachment{
						Data:   audioData,
						Format: format,
					}
				}
			case "at":
				if qq, ok := data["qq"].(string); ok && qq != "" {
					if qq == "all" {
						textParts = append(textParts, "@所有人")
					} else if p.selfID != 0 && qq == strconv.FormatInt(p.selfID, 10) {
						// Skip @bot itself so commands like /new still work
					} else {
						textParts = append(textParts, "@"+qq)
					}
				}
			}
		}
	default:
		// raw_message fallback (string with CQ codes)
		if raw, ok := payload["raw_message"].(string); ok {
			textParts = append(textParts, stripCQCodes(raw))
		}
	}

	return strings.TrimSpace(strings.Join(textParts, "")), images, audio, fromVoice
}

// Reply sends a message as a reply to an incoming message.
func (p *Platform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.Send(ctx, replyCtx, content)
}

// Send sends a message to the conversation identified by replyCtx.
func (p *Platform) Send(ctx context.Context, replyCtx any, content string) error {
	rctx, ok := replyCtx.(*replyContext)
	if !ok {
		return fmt.Errorf("qq: invalid reply context")
	}

	// Mute check: skip sending if bot is muted in the group
	if p.checkMute && rctx.messageType == "group" && rctx.groupID != 0 {
		if p.isMutedInGroup(rctx.groupID) {
			slog.Debug("qq: skip send, bot is muted in group", "group_id", rctx.groupID)
			return nil
		}
	}

	var emojiID string
	cleanContent := content

	// Strip [emo*:...] markers (AI may output various formats)
	if idx := strings.LastIndex(content, "[emo"); idx >= 0 {
		if end := strings.Index(content[idx:], "]"); end > 4 {
			cleanContent = strings.TrimSpace(content[:idx] + content[idx+end+1:])
			raw := content[idx+4 : idx+end]
			if _, after, found := strings.Cut(raw, ":"); found {
				emojiID = strings.TrimSpace(after)
			}
			slog.Debug("qq: stripped emoji marker", "raw", raw, "clean", truncateText(cleanContent, 60))
		}
	}

	params := map[string]any{}

	if rctx.messageType == "group" {
		params["group_id"] = rctx.groupID

		// Send as reply with quote
		if p.replyWithQuote && rctx.messageID != 0 {
			params["message"] = []map[string]any{
				{"type": "reply", "data": map[string]any{"id": strconv.FormatInt(int64(rctx.messageID), 10)}},
				{"type": "text", "data": map[string]any{"text": cleanContent}},
			}
		} else {
			params["message"] = cleanContent
		}
		_, err := p.callAPI("send_group_msg", params)
		if err == nil && p.emojiLike && emojiID != "" && rctx.messageID != 0 {
			p.addEmojiLike(int64(rctx.messageID), emojiID)
		}
		return err
	}

	params["user_id"] = rctx.userID
	params["message"] = cleanContent
	_, err := p.callAPI("send_private_msg", params)
	return err
}

// SendImage sends an image to the conversation.
// Implements core.ImageSender.
func (p *Platform) SendImage(ctx context.Context, replyCtx any, img core.ImageAttachment) error {
	rctx, ok := replyCtx.(*replyContext)
	if !ok {
		return fmt.Errorf("qq: SendImage: invalid reply context type %T", replyCtx)
	}

	b64 := base64.StdEncoding.EncodeToString(img.Data)
	segments := []map[string]any{
		{"type": "image", "data": map[string]any{"file": "base64://" + b64}},
	}

	params := map[string]any{
		"message": segments,
	}

	if rctx.messageType == "group" {
		params["group_id"] = rctx.groupID
		_, err := p.callAPI("send_group_msg", params)
		if err != nil {
			return fmt.Errorf("qq: send image: %w", err)
		}
		return nil
	}

	params["user_id"] = rctx.userID
	_, err := p.callAPI("send_private_msg", params)
	if err != nil {
		return fmt.Errorf("qq: send image: %w", err)
	}
	return nil
}

var _ core.ImageSender = (*Platform)(nil)

func (p *Platform) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.stopCronJobs()
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

func (p *Platform) resolveGroupName(groupID int64) string {
	if groupID == 0 {
		return ""
	}
	fallback := strconv.FormatInt(groupID, 10)
	if cached, ok := p.groupNameCache.Load(fallback); ok {
		return cached.(string)
	}
	result, err := p.callAPI("get_group_info", map[string]any{"group_id": groupID})
	if err != nil {
		slog.Debug("qq: resolve group name failed", "group_id", groupID, "error", err)
		return fallback
	}
	name, _ := result["group_name"].(string)
	if name != "" {
		p.groupNameCache.Store(fallback, name)
		return name
	}
	return fallback
}

// ── OneBot API call via WebSocket ───────────────────────────────

func (p *Platform) callAPI(action string, params map[string]any) (map[string]any, error) {
	raw, err := p.callAPIRaw(action, params)
	if err != nil {
		return nil, err
	}
	// Some actions answer with an array (get_rkey); callers that only expect an
	// object get a nil map rather than an error.
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result, nil
}

// callAPIRaw performs the request and returns the raw "data" field, whatever its
// JSON shape.
func (p *Platform) callAPIRaw(action string, params map[string]any) (json.RawMessage, error) {
	seq := p.echoSeq.Add(1)
	echo := strconv.FormatInt(seq, 10)

	req := map[string]any{
		"action": action,
		"echo":   echo,
	}
	if params != nil {
		req["params"] = params
	}

	ch := make(chan json.RawMessage, 1)
	p.echoCh.Store(echo, ch)
	defer p.echoCh.Delete(echo)

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	conn := p.conn
	if conn == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("qq: %s: not connected", action)
	}
	err = conn.WriteMessage(websocket.TextMessage, data)
	p.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("qq: ws write: %w", err)
	}

	select {
	case raw := <-ch:
		var resp struct {
			Status  string          `json:"status"`
			RetCode int             `json:"retcode"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			return nil, fmt.Errorf("qq: invalid API response")
		}
		if resp.RetCode != 0 {
			return nil, fmt.Errorf("qq: API %s failed (retcode=%d)", action, resp.RetCode)
		}
		return resp.Data, nil

	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("qq: API %s timeout", action)
	}
}

// ── Group features ────────────────────────────────────────────

// handleGroupRecall handles group_recall events — routes through AI for dynamic response.
func (p *Platform) handleGroupRecall(payload map[string]any) {
	if !p.recallComment {
		return
	}
	if !p.scopeAllows("group") {
		return
	}

	groupID := jsonInt64(payload, "group_id")
	operatorID := jsonInt64(payload, "operator_id")
	messageID := jsonInt64(payload, "message_id")

	if groupID == 0 || !p.isGroupAllowed(groupID) {
		return
	}

	if ts, ok := payload["time"].(float64); ok && ts > 0 {
		if core.IsOldMessage(time.Unix(int64(ts), 0)) {
			return
		}
	}

	// Bot recalling its own message — ignore
	if operatorID == p.selfID {
		return
	}

	// Build recall context for the AI
	prompt := "有人撤回了消息"
	if entry := p.lookupMessage(messageID); entry != nil && entry.content != "" {
		prompt = "有人撤回了消息，内容大概是：「" + truncateText(entry.content, 50) + "」"
	}

	sessionKey := fmt.Sprintf("qq:g:%d", groupID)
	rctx := &replyContext{messageType: "group", groupID: groupID, persona: p.personaFor(sessionKey)}
	msg := &core.Message{
		SessionKey:   sessionKey,
		Platform:     "qq",
		MessageID:    fmt.Sprintf("recall_%d_%d", groupID, time.Now().UnixMilli()),
		UserID:       strconv.FormatInt(p.selfID, 10),
		ChatName:     p.resolveGroupName(groupID),
		Content:      prompt,
		ExtraContent: p.makeGroupExtraContent(sessionKey),
		ReplyCtx:     rctx,
	}
	slog.Debug("qq: group recall handled", "group", groupID, "message_id", messageID)
	p.handler(p, msg)
}

// trackMessage caches a recent message for recall content lookup.
func (p *Platform) trackMessage(messageID int64, userID int64, content string) {
	p.recentMessagesMu.Lock()
	defer p.recentMessagesMu.Unlock()

	// Evict old entries if cache is too large
	if len(p.recentMessages) >= 500 {
		oldest := int64(0)
		var oldestKey int64
		for id, entry := range p.recentMessages {
			if oldest == 0 || entry.timestamp.Unix() < oldest {
				oldest = entry.timestamp.Unix()
				oldestKey = id
			}
		}
		delete(p.recentMessages, oldestKey)
	}

	p.recentMessages[messageID] = &recallEntry{
		userID:    userID,
		content:   content,
		timestamp: time.Now(),
	}
}

// lookupMessage retrieves a cached message for recall detection.
func (p *Platform) lookupMessage(messageID int64) *recallEntry {
	p.recentMessagesMu.Lock()
	defer p.recentMessagesMu.Unlock()
	entry, ok := p.recentMessages[messageID]
	if !ok {
		return nil
	}
	// Expire entries older than 5 minutes
	if time.Since(entry.timestamp) > 5*time.Minute {
		delete(p.recentMessages, messageID)
		return nil
	}
	return entry
}

// addEmojiLike adds an emoji reaction to a message (NapCat extended API).
// If emojiID is empty, uses the configured default ("76" = thumbs up).
// Skips if already reacted to this message (checked via emojiReacted map).
func (p *Platform) addEmojiLike(messageID int64, emojiID string) {
	if emojiID == "" {
		emojiID = p.emojiLikeDef
	}
	if emojiID == "" {
		emojiID = "76" // default thumbs up
	}

	msgKey := strconv.FormatInt(messageID, 10) + ":" + emojiID
	if _, loaded := p.emojiReacted.LoadOrStore(msgKey, true); loaded {
		return // already reacted with this emoji
	}

	slog.Debug("qq: set_msg_emoji_like", "message_id", messageID, "emoji_id", emojiID)
	_, err := p.callAPI("set_msg_emoji_like", map[string]any{
		"message_id": messageID,
		"emoji_id":   emojiID,
	})
	if err != nil {
		slog.Debug("qq: set_msg_emoji_like failed", "error", err)
	}
}

// ── Mute check ────────────────────────────────────────────────

// isMutedInGroup checks if the bot is muted in the given group, with caching.
func (p *Platform) isMutedInGroup(groupID int64) bool {
	cacheKey := strconv.FormatInt(groupID, 10)

	// Check cache first
	if cached, ok := p.muteCache.Load(cacheKey); ok {
		entry := cached.(*muteCacheEntry)
		if time.Since(entry.cachedAt) < 15*time.Second {
			return entry.isMuted
		}
	}

	// Fetch from API
	result, err := p.callAPI("get_group_member_info", map[string]any{
		"group_id": groupID,
		"user_id":  p.selfID,
	})
	if err != nil {
		slog.Debug("qq: check mute failed", "group_id", groupID, "error", err)
		return false
	}

	shutUpTs, _ := result["shut_up_timestamp"].(float64)
	until := time.Unix(int64(shutUpTs), 0)
	isMuted := shutUpTs > 0 && until.After(time.Now())

	p.muteCache.Store(cacheKey, &muteCacheEntry{
		isMuted:  isMuted,
		until:    until,
		cachedAt: time.Now(),
	})

	if isMuted {
		slog.Debug("qq: bot is muted in group", "group_id", groupID, "until", until)
	}
	return isMuted
}

// ── Cron scheduled messages ───────────────────────────────────

func (p *Platform) startCronJobs() {
	if len(p.cronJobs) == 0 {
		return
	}

	p.cronScheduler = cron.New()
	for i, job := range p.cronJobs {
		idx := i
		j := job
		_, err := p.cronScheduler.AddFunc(j.Cron, func() {
			if j.Message != "" {
				p.executeCronMessage(j)
				return
			}
			p.executeCronJob(j.GroupID, j.Prompt, idx)
		})
		if err != nil {
			slog.Error("qq: invalid cron expression", "cron", j.Cron, "error", err)
			continue
		}
		if j.Message != "" {
			slog.Info("qq: cron message registered",
				"cron", j.Cron, "group", j.GroupID, "at", j.At, "message", truncateText(j.Message, 40))
			continue
		}
		slog.Info("qq: cron prompt registered", "cron", j.Cron, "group", j.GroupID, "prompt", truncateText(j.Prompt, 40))
	}
	p.cronScheduler.Start()
}

func (p *Platform) stopCronJobs() {
	if p.cronScheduler != nil {
		p.cronScheduler.Stop()
	}
}

// executeCronMessage sends a fixed group message on a schedule, optionally
// @-mentioning someone first. It deliberately bypasses the agent: jobs like this
// exist to poke another bot's command (e.g. "@otherbot /签到"), so the text has
// to go out exactly as configured, and the mention has to be a real at-segment
// rather than plain text that no bot would recognise.
func (p *Platform) executeCronMessage(job cronJobConfig) {
	if !p.scopeAllows("group") {
		slog.Debug("qq: skipping scheduled message, groups are out of scope", "group", job.GroupID)
		return
	}
	if p.checkMute && p.isMutedInGroup(job.GroupID) {
		slog.Debug("qq: skipping scheduled message, bot is muted", "group", job.GroupID)
		return
	}

	segments := make([]map[string]any, 0, 2)
	if job.At != 0 {
		segments = append(segments, map[string]any{
			"type": "at",
			"data": map[string]any{"qq": strconv.FormatInt(job.At, 10)},
		})
	}
	segments = append(segments, map[string]any{
		"type": "text",
		"data": map[string]any{"text": job.Message},
	})

	if _, err := p.callAPI("send_group_msg", map[string]any{
		"group_id": job.GroupID,
		"message":  segments,
	}); err != nil {
		slog.Error("qq: scheduled message failed", "group", job.GroupID, "error", err)
		return
	}
	slog.Info("qq: scheduled message sent",
		"group", job.GroupID, "at", job.At, "message", truncateText(job.Message, 40))
}

func (p *Platform) executeCronJob(groupID int64, prompt string, _ int) {
	if !p.scopeAllows("group") {
		slog.Debug("qq: skipping cron job, groups are out of scope", "group", groupID)
		return
	}

	// Route through the AI handler so content is dynamically generated
	sessionKey := fmt.Sprintf("qq:g:%d", groupID)
	rctx := &replyContext{messageType: "group", groupID: groupID, persona: p.personaFor(sessionKey)}
	msg := &core.Message{
		SessionKey:   sessionKey,
		Platform:     "qq",
		MessageID:    fmt.Sprintf("cron_%d_%d", groupID, time.Now().UnixMilli()),
		UserID:       strconv.FormatInt(p.selfID, 10),
		ChatName:     p.resolveGroupName(groupID),
		Content:      prompt,
		ExtraContent: p.makeGroupExtraContent(sessionKey),
		ReplyCtx:     rctx,
	}
	slog.Info("qq: cron job executing", "group", groupID, "prompt", truncateText(prompt, 60))
	p.handler(p, msg)
}

// defaultPersona is used when a group session has no /persona override.
const defaultPersona = "贴吧老哥"

// personaFor returns the persona tag for a session, falling back to the default.
func (p *Platform) personaFor(sessionKey string) string {
	if tag, ok := p.personaMap.Load(sessionKey); ok {
		if s, ok := tag.(string); ok && s != "" {
			return s
		}
	}
	return defaultPersona
}

func (p *Platform) makeGroupExtraContent(sessionKey string) string {
	return "[群聊消息][" + p.personaFor(sessionKey) + "]"
}

// ── Message buffer ──────────────────────────────

func (p *Platform) addToBuffer(sessionKey string, userName string, userID int64, text string) {
	raw, _ := p.msgBufferMap.LoadOrStore(sessionKey, &msgBuffer{lines: []string{}, start: time.Now()})
	buf := raw.(*msgBuffer)

	// Format: "userName(userID): text"
	if userName != "" {
		buf.lines = append(buf.lines, fmt.Sprintf("%s(%d): %s", userName, userID, text))
	} else {
		buf.lines = append(buf.lines, fmt.Sprintf("(%d): %s", userID, text))
	}
}

// flushBuffer returns all buffered messages as a single string and clears the buffer.
func (p *Platform) flushBuffer(sessionKey string) string {
	raw, ok := p.msgBufferMap.Load(sessionKey)
	if !ok {
		return ""
	}
	buf := raw.(*msgBuffer)

	// Check freshness: if buffer is older than 30 min, discard
	if time.Since(buf.start) > 30*time.Minute {
		p.msgBufferMap.Delete(sessionKey)
		return ""
	}

	if len(buf.lines) <= 1 {
		// Only current message, no need for special formatting
		p.msgBufferMap.Delete(sessionKey)
		return ""
	}

	result := "以下是你上次回复之后的群聊记录：\n\n"
	for _, line := range buf.lines {
		result += line + "\n"
	}
	p.msgBufferMap.Delete(sessionKey)
	return result
}

// ── Reply probability ───────────────────────────

// replyStateFor returns the per-session throttling state, creating it on first use.
func (p *Platform) replyStateFor(sessionKey string) *replyState {
	raw, _ := p.replyStateMap.LoadOrStore(sessionKey, &replyState{})
	return raw.(*replyState)
}

// inCooldown reports whether the bot spoke too recently to speak again here.
// Only replies to messages that did not address the bot advance the clock, so
// being @-mentioned is never suppressed.
func (p *Platform) inCooldown(rs *replyState, now time.Time) bool {
	return p.replyCooldown > 0 && !rs.lastReplyTime.IsZero() &&
		now.Sub(rs.lastReplyTime) < time.Duration(p.replyCooldown)*time.Second
}

func (p *Platform) shouldReply(sessionKey string, text string) bool {
	rs := p.replyStateFor(sessionKey)

	now := time.Now()

	// Cooldown: stay quiet for a while after speaking so the bot does not weigh in
	// on every ambient message. Only messages that did not address the bot reach
	// this point, so a direct @-mention is never suppressed by it.
	if p.inCooldown(rs, now) {
		rs.skipCount++
		slog.Info("qq: skip, cooldown",
			"session", sessionKey, "since_last_reply_s", int(now.Sub(rs.lastReplyTime).Seconds()),
			"text", truncateText(text, 20))
		return false
	}

	// Calculate probability
	P := float64(p.replyProbability)

	// 1. Skip penalty: each skipped message reduces probability
	skipDeduction := float64(rs.skipCount) * float64(p.replySkipPenalty)
	P -= skipDeduction

	// 2. Cold boost: silence after last reply increases probability
	if !rs.lastReplyTime.IsZero() {
		silence := now.Sub(rs.lastReplyTime)
		if silence > 30*time.Second {
			extra := float64(silence.Seconds()-30) / 10.0 * float64(p.replyColdBoost)
			P += extra
		}
	}

	// Clamp
	minP := float64(p.replyProbMin)
	maxP := float64(p.replyProbMax)
	if P < minP {
		P = minP
	}
	if P > maxP {
		P = maxP
	}

	// Content-based modifiers
	// Questions: double probability
	if strings.Contains(text, "?") || strings.Contains(text, "？") ||
		strings.Contains(text, "吗") || strings.Contains(text, "什么") ||
		strings.Contains(text, "啥") || strings.Contains(text, "怎么") {
		P *= 2
		if P > maxP {
			P = maxP
		}
	}

	// Short / pure emoji: halve probability
	if len([]rune(text)) <= 2 {
		P /= 2
		if P < minP {
			P = minP
		}
	}

	// Roll the dice
	replied := rand.Float64()*100 < P
	slog.Info("qq: reply prob",
		"prob", int(P),
		"skip", rs.skipCount,
		"text", truncateText(text, 20),
		"reply", replied)

	if replied {
		// Replied: reset state
		rs.skipCount = 0
		rs.lastReplyTime = now
		return true
	}

	// Skipped: increment counter (state persists for next message)
	rs.skipCount++
	return false
}

// ── Voice transcription ─────────────────────────

// transcribeAudio downloads an audio file and runs Whisper via FFmpeg pipe to get text.
func (p *Platform) transcribeAudio(url string) string {
	data, _, err := downloadFile(url)
	if err != nil {
		slog.Info("qq: whisper download failed", "error", err)
		return ""
	}

	// Write raw audio to temp file
	rawFile := filepath.Join(os.TempDir(), fmt.Sprintf("qq_raw_%d", time.Now().UnixNano()))
	if err := os.WriteFile(rawFile, data, 0644); err != nil {
		slog.Info("qq: whisper write failed", "error", err)
		return ""
	}
	defer os.Remove(rawFile)

	// Convert to WAV via FFmpeg (AMR/SILK → PCM)
	wavFile := rawFile + ".wav"
	defer os.Remove(wavFile)

	ffmpeg := exec.Command("ffmpeg", "-y", "-i", rawFile, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wavFile)
	if err := ffmpeg.Run(); err != nil {
		slog.Info("qq: whisper ffmpeg convert failed", "error", err)
		return ""
	}

	outFile := rawFile + ".txt"
	defer os.Remove(outFile)

	cmd := exec.Command("whisper-cli",
		"-m", p.whisperModel,
		"-f", wavFile,
		"-of", rawFile,
		"-otxt",
		"-l", "zh",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("qq: whisper failed", "error", err, "output", string(output))
		return ""
	}

	transcript, err := os.ReadFile(outFile)
	if err != nil {
		slog.Info("qq: whisper read output failed", "error", err)
		return ""
	}

	result := strings.TrimSpace(string(transcript))
	slog.Info("qq: whisper transcript", "text", truncateText(result, 60))
	return result
}

// ── Voice (TTS) ─────────────────────────────────

// SendAudio sends a synthesized voice message. Synthesis itself is done by the
// engine's TTS pipeline; the platform only converts to a format NapCat accepts
// and hands it to OneBot. Implements core.AudioSender.
func (p *Platform) SendAudio(ctx context.Context, replyCtx any, audio []byte, format string) error {
	rctx, ok := replyCtx.(*replyContext)
	if !ok {
		return fmt.Errorf("qq: SendAudio: invalid reply context type %T", replyCtx)
	}
	if len(audio) == 0 {
		return fmt.Errorf("qq: SendAudio: empty audio")
	}

	// Providers emit WAV or MP3; NapCat's record segment wants MP3.
	data := audio
	if !strings.EqualFold(format, "mp3") {
		converted, err := core.ConvertAudioToMP3(ctx, audio, format)
		if err != nil {
			return fmt.Errorf("qq: convert %s audio to mp3: %w", format, err)
		}
		data = converted
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	params := map[string]any{
		"message": []map[string]any{
			{"type": "record", "data": map[string]any{"file": "base64://" + b64}},
		},
	}

	if rctx.messageType == "group" {
		params["group_id"] = rctx.groupID
		_, err := p.callAPI("send_group_msg", params)
		return err
	}
	params["user_id"] = rctx.userID
	_, err := p.callAPI("send_private_msg", params)
	return err
}

// SelectVoice returns the TTS voice configured for the speaker's current
// persona, or "" to fall back to the global [tts] voice.
// Implements core.VoiceSelector.
func (p *Platform) SelectVoice(replyCtx any) string {
	rctx, ok := replyCtx.(*replyContext)
	if !ok || rctx.persona == "" {
		return ""
	}
	return p.personaVoices[rctx.persona]
}

// AllowVoice thins out voice replies by probability so the group does not get
// spoken word for every message. A reply that answers a voice message is always
// spoken; otherwise voiceProbability applies (0 = never gate, i.e. always allow).
// Private chats are the operator's own workspace and stay text-only.
// Implements core.VoiceGate.
func (p *Platform) AllowVoice(replyCtx any, _ string, fromVoice bool) bool {
	rctx, ok := replyCtx.(*replyContext)
	if !ok || rctx.messageType != "group" {
		return false
	}
	if fromVoice || p.voiceProbability <= 0 || p.voiceProbability >= 100 {
		return true
	}
	return rand.Intn(100) < p.voiceProbability
}

var (
	_ core.AudioSender   = (*Platform)(nil)
	_ core.VoiceSelector = (*Platform)(nil)
	_ core.VoiceGate     = (*Platform)(nil)
)

// ── Chat scope ──────────────────────────────────

// currentScope returns the configured chat scope.
func (p *Platform) currentScope() string {
	p.scopeMu.Lock()
	defer p.scopeMu.Unlock()
	if p.chatScope == "" {
		return chatScopeAll
	}
	return p.chatScope
}

// setScope switches the chat scope and persists it so the change survives a restart.
func (p *Platform) setScope(scope string) {
	p.scopeMu.Lock()
	p.chatScope = scope
	path := p.scopePath
	p.scopeMu.Unlock()

	if path == "" {
		return
	}
	if err := saveChatScope(path, scope); err != nil {
		slog.Warn("qq: persist chat scope failed", "path", path, "error", err)
	}
}

// scopeAllows reports whether the bot answers in this kind of chat.
func (p *Platform) scopeAllows(msgType string) bool {
	switch p.currentScope() {
	case chatScopeGroup:
		return msgType == "group"
	case chatScopePrivate:
		return msgType == "private"
	default:
		return true
	}
}

// scopeBlocks reports whether a message should be dropped for being outside the
// configured scope. /scope always gets through so an admin can switch back even
// when the chat they are typing in is currently out of scope.
func (p *Platform) scopeBlocks(msgType, text string) bool {
	if p.scopeAllows(msgType) {
		return false
	}
	return !isCommand(text, "/scope")
}

// handleScopeCommand implements the admin-only /scope command.
func (p *Platform) handleScopeCommand(rctx *replyContext, userID int64, text string) {
	reply := func(msg string) {
		if err := p.Reply(context.Background(), rctx, msg); err != nil {
			slog.Warn("qq: reply to /scope failed", "error", err)
		}
	}

	if !p.isAdmin(userID) {
		if p.adminIDs == "" {
			reply("❌ 还没配置 admin_ids，没人能切换生效范围。")
		} else {
			reply("❌ 只有管理员才能切换生效范围。")
		}
		return
	}

	arg := strings.TrimSpace(strings.TrimSpace(text)[len("/scope"):])
	if arg == "" {
		reply(fmt.Sprintf("当前生效范围: %s\n用法: /scope all|group|private（全部/群聊/私聊）", scopeLabel(p.currentScope())))
		return
	}

	scope := parseChatScope(arg)
	if scope == "" {
		reply("❌ 参数无效。用法: /scope all|group|private（全部/群聊/私聊）")
		return
	}

	p.setScope(scope)
	slog.Info("qq: chat scope switched", "scope", scope, "by", userID)
	reply("✅ 生效范围已切换为: " + scopeLabel(scope))
}

// parseChatScope normalizes a scope value, returning "" when it is not recognized.
func parseChatScope(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "all", "both", "全部", "都":
		return chatScopeAll
	case "group", "群聊", "群":
		return chatScopeGroup
	case "private", "单聊", "私聊", "私":
		return chatScopePrivate
	}
	return ""
}

func scopeLabel(scope string) string {
	switch scope {
	case chatScopeGroup:
		return "仅群聊"
	case chatScopePrivate:
		return "仅私聊"
	default:
		return "私聊+群聊"
	}
}

// isControlCommand reports whether text is a command rather than chat: either one
// this adapter handles itself or one the engine dispatches. Anything else,
// including "/" text the engine does not recognize, is ordinary chat and is
// subject to the reply throttle.
func isControlCommand(text string) bool {
	return isCommand(text, "/scope") || isCommand(text, "/persona") || core.IsBuiltinCommand(text)
}

// isCommand reports whether text invokes the given command, either bare or
// followed by a space-separated argument.
func isCommand(text, command string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, command) {
		return false
	}
	rest := text[len(command):]
	return rest == "" || rest[0] == ' '
}

// rawTextOf extracts text segments only, without downloading attachments, so the
// chat-scope gate can inspect a message cheaply.
func rawTextOf(payload map[string]any) string {
	var b strings.Builder
	switch msg := payload["message"].(type) {
	case []any:
		for _, seg := range msg {
			s, ok := seg.(map[string]any)
			if !ok || s["type"] != "text" {
				continue
			}
			if data, ok := s["data"].(map[string]any); ok {
				if t, ok := data["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
	default:
		if raw, ok := payload["raw_message"].(string); ok {
			b.WriteString(stripCQCodes(raw))
		}
	}
	return strings.TrimSpace(b.String())
}

// ── Chat scope persistence ──────────────────────

type chatScopeState struct {
	ChatScope string `json:"chat_scope"`
}

// chatScopeStatePath returns where the /scope override is stored, or "" when the
// data dir and project name were not injected (e.g. in unit tests).
func chatScopeStatePath(opts map[string]any) string {
	dataDir := strings.TrimSpace(toString(opts["cc_data_dir"]))
	project := strings.TrimSpace(toString(opts["cc_project"]))
	if dataDir == "" || project == "" {
		return ""
	}
	return filepath.Join(dataDir, "qq", sanitizePathPart(project), "chat_scope.json")
}

func loadChatScope(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("qq: read chat scope state failed", "path", path, "error", err)
		}
		return ""
	}
	var state chatScopeState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("qq: parse chat scope state failed", "path", path, "error", err)
		return ""
	}
	return parseChatScope(state.ChatScope)
}

func saveChatScope(path, scope string) error {
	data, err := json.MarshalIndent(chatScopeState{ChatScope: scope}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// sanitizePathPart keeps letters, digits and a few punctuation marks so project
// names (including non-ASCII ones) map to a safe single path segment.
func sanitizePathPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// ── Helpers ──

type replyContext struct {
	messageType string // "private" or "group"
	userID      int64
	groupID     int64
	messageID   int32
	persona     string // current persona tag for TTS
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// qq:{userID}, qq:{groupID}:{userID} or qq:g:{groupID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "qq" {
		return nil, fmt.Errorf("qq: invalid session key %q", sessionKey)
	}
	if len(parts) == 3 {
		persona := p.personaFor(sessionKey)
		if parts[1] == "g" {
			gid, _ := strconv.ParseInt(parts[2], 10, 64)
			return &replyContext{messageType: "group", groupID: gid, persona: persona}, nil
		}
		gid, _ := strconv.ParseInt(parts[1], 10, 64)
		uid, _ := strconv.ParseInt(parts[2], 10, 64)
		return &replyContext{messageType: "group", groupID: gid, userID: uid, persona: persona}, nil
	}
	uid, _ := strconv.ParseInt(parts[1], 10, 64)
	return &replyContext{messageType: "private", userID: uid}, nil
}

func (p *Platform) isAllowed(userID int64) bool {
	if p.allowFrom == "" || p.allowFrom == "*" {
		return true
	}
	uid := strconv.FormatInt(userID, 10)
	for _, allowed := range strings.Split(p.allowFrom, ",") {
		if strings.TrimSpace(allowed) == uid {
			return true
		}
	}
	return false
}

func (p *Platform) isGroupAllowed(groupID int64) bool {
	if p.groupAllow == "" || p.groupAllow == "*" {
		return true
	}
	gid := strconv.FormatInt(groupID, 10)
	for _, allowed := range strings.Split(p.groupAllow, ",") {
		if strings.TrimSpace(allowed) == gid {
			return true
		}
	}
	return false
}

func (p *Platform) isAdmin(userID int64) bool {
	if p.adminIDs == "" {
		return false
	}
	uid := strconv.FormatInt(userID, 10)
	for _, id := range strings.Split(p.adminIDs, ",") {
		if strings.TrimSpace(id) == uid {
			return true
		}
	}
	return false
}

// isBotMentioned checks if the message contains an @mention of the bot itself.
func (p *Platform) isBotMentioned(payload map[string]any) bool {
	selfIDStr := strconv.FormatInt(p.selfID, 10)
	switch msg := payload["message"].(type) {
	case []any:
		for _, seg := range msg {
			s, ok := seg.(map[string]any)
			if !ok {
				continue
			}
			if s["type"] == "at" {
				if data, ok := s["data"].(map[string]any); ok {
					if qq, ok := data["qq"].(string); ok {
						if qq == selfIDStr {
							return true
						}
					}
				}
			}
		}
	default:
		if raw, ok := payload["raw_message"].(string); ok {
			if strings.Contains(raw, "[CQ:at,qq="+selfIDStr+"]") {
				return true
			}
		}
	}
	return false
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// optInt reads an integer option. TOML decodes integers as int64, while tests
// and programmatic callers use int, so both must be accepted.
func optInt(opts map[string]any, key string) int {
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// optIntOr reads an integer option, falling back to def only when the key is
// absent, so an explicit 0 is preserved.
func optIntOr(opts map[string]any, key string, def int) int {
	if _, ok := opts[key]; !ok {
		return def
	}
	return optInt(opts, key)
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int64:
		return n, true
	}
	return 0, false
}

func jsonInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func stripCQCodes(s string) string {
	var result strings.Builder
	for len(s) > 0 {
		idx := strings.Index(s, "[CQ:")
		if idx < 0 {
			result.WriteString(s)
			break
		}
		result.WriteString(s[:idx])
		end := strings.Index(s[idx:], "]")
		if end < 0 {
			break
		}
		s = s[idx+end+1:]
	}
	return result.String()
}

func truncateText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func downloadFile(url string) ([]byte, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

// fetchImage obtains an image segment's bytes.
//
// The url in a message event carries a QQ CDN rkey that can already be stale by
// the time the event arrives — fetching it returns HTTP 400, which is why images
// used to arrive as a 70-byte {"retcode":-5503007,"retmsg":"download url has
// expired"} payload. The url is therefore rebuilt with a current rkey first, and
// the event url is only a fallback.
func (p *Platform) fetchImage(data map[string]any, chatKind string) (core.ImageAttachment, bool) {
	file, _ := data["file"].(string)
	eventURL, _ := data["url"].(string)
	slog.Debug("qq: image segment", "file", file, "url_len", len(eventURL))

	if rebuilt := p.withFreshRKey(eventURL, chatKind); rebuilt != "" && rebuilt != eventURL {
		if img, ok := downloadImage(rebuilt); ok {
			return img, true
		}
	}
	if eventURL != "" {
		if img, ok := downloadImage(eventURL); ok {
			return img, true
		}
	}
	return core.ImageAttachment{}, false
}

// withFreshRKey rebuilds a QQ CDN download url using a currently valid rkey from
// get_rkey. Returns "" when the url is not a CDN link or no rkey is available.
func (p *Platform) withFreshRKey(rawURL, chatKind string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	q := u.Query()
	if q.Get("fileid") == "" {
		return ""
	}
	rkey := p.cdnRKey(chatKind)
	if rkey == "" {
		return ""
	}
	q.Set("rkey", rkey)
	u.RawQuery = q.Encode()
	return u.String()
}

// cdnRKey returns a cached rkey for the given chat kind, refreshing it from
// get_rkey when it is missing or close to expiry.
func (p *Platform) cdnRKey(kind string) string {
	p.rkeyMu.Lock()
	defer p.rkeyMu.Unlock()

	if p.rkeyMap == nil {
		p.rkeyMap = map[string]rkeyEntry{}
	}
	if e, ok := p.rkeyMap[kind]; ok && e.value != "" && time.Since(e.fetchedAt) < rkeyRefreshInterval {
		return e.value
	}

	raw, err := p.callAPIRaw("get_rkey", nil)
	if err != nil {
		slog.Warn("qq: get_rkey failed", "error", err)
		return p.rkeyMap[kind].value
	}
	var list []struct {
		Type string `json:"type"`
		RKey string `json:"rkey"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		slog.Warn("qq: get_rkey: unexpected response", "error", err)
		return p.rkeyMap[kind].value
	}
	now := time.Now()
	for _, e := range list {
		if e.Type == "" || e.RKey == "" {
			continue
		}
		// NapCat hands the value back with the query prefix already attached.
		p.rkeyMap[e.Type] = rkeyEntry{value: strings.TrimPrefix(e.RKey, "&rkey="), fetchedAt: now}
	}
	slog.Debug("qq: refreshed CDN rkeys", "kinds", len(list))
	return p.rkeyMap[kind].value
}

func downloadImage(url string) (core.ImageAttachment, bool) {
	data, _, err := downloadFile(url)
	if err != nil {
		slog.Warn("qq: image download failed", "error", err)
		return core.ImageAttachment{}, false
	}
	return validateImage(data)
}

// validateImage rejects anything that is not really an image. QQ serves its
// errors with HTTP 200 and a JSON body such as
// {"retcode":-5503007,"retmsg":"download url has expired"}, which would otherwise
// be handed to the agent as a picture of an error message.
func validateImage(data []byte) (core.ImageAttachment, bool) {
	if len(data) == 0 {
		return core.ImageAttachment{}, false
	}
	// Sniff the bytes rather than trusting Content-Type: the error payload may
	// even be labelled image/png.
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		slog.Warn("qq: image download returned non-image content",
			"detected", mime, "bytes", len(data), "body", truncateText(string(data), 120))
		return core.ImageAttachment{}, false
	}
	return core.ImageAttachment{MimeType: mime, Data: data}, true
}

// chatKind reports whether a payload came from a group, for choosing which CDN
// rkey applies.
func chatKind(payload map[string]any) string {
	if t, _ := payload["message_type"].(string); t == "group" {
		return "group"
	}
	return "private"
}
