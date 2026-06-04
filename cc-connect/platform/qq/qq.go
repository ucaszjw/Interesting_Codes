package qq

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
}

// cronJobConfig defines a scheduled prompt sent to the AI for dynamic message generation.
type cronJobConfig struct {
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
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
	emojiLikeDef, _ := opts["emoji_like_id"].(string)

	// Cron jobs: prompts sent to the AI for dynamic generation
	var cronJobs []cronJobConfig
	if raw, ok := opts["cron_jobs"].([]any); ok {
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				cron := toString(m["cron"])
				prompt := toString(m["prompt"])
				gid, _ := toInt64(m["group_id"])
				if cron != "" && prompt != "" && gid != 0 {
					cronJobs = append(cronJobs, cronJobConfig{Cron: cron, Prompt: prompt, GroupID: gid})
				}
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

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

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

		// Otherwise it's an event
		postType, _ := payload["post_type"].(string)
		switch postType {
		case "message":
			p.handleMessage(payload)
		case "notice":
			p.handleNotice(payload)
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
	text, images, audio := p.parseMessage(payload)
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

	rctx := &replyContext{
		messageType: msgType,
		userID:      userID,
		groupID:     groupID,
		messageID:   int32(messageID),
	}

	var chatName string
	if msgType == "group" {
		chatName = p.resolveGroupName(groupID)
	}

	// Set ExtraContent to indicate chat type so the agent can differentiate behavior
	var extraContent string
	if msgType == "group" || (p.toolAdminOnly && !p.isAdmin(userID)) {
		extraContent = "[群聊消息]"
		// Add persona tag after [群聊消息] (before sender info)
		if msgType == "group" {
			if tag, ok := p.personaMap.Load(sessionKey); ok {
				extraContent = "[群聊消息][" + tag.(string) + "]" 
			} else {
				extraContent = "[群聊消息][贴吧老哥]"
			}
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

	// Block non-admin users from approving tool permission requests
	if p.toolAdminOnly && !p.isAdmin(userID) && isPermissionApproval(text) {
		p.Reply(context.Background(), rctx, "❌ 只有管理员才能批准操作，你一边呆着去。")
		return
	}

	// Handle /persona command (persona switching)
	if msgType == "group" && strings.HasPrefix(text, "/persona") && (len(text) == len("/persona") || text[len("/persona")] == ' ') {
		name := strings.TrimSpace(text[len("/persona"):])
		validPersonas := map[string]string{
			"tieba": "贴吧老哥", "老哥": "贴吧老哥",
			"neko": "猫娘", "catgirl": "猫娘",
			"cadre": "老干部", "老干": "老干部",
			"simp": "母狗", "母狗": "母狗",
			"cute": "萌妹", "萌妹": "萌妹",
			"straight": "直男", "直男": "直男",
		}
		if name == "" {
			available := "贴吧老哥/tieba, 猫娘/neko, 老干部/cadre, 母狗/simp, 萌妹/cute, 直男/straight"
			p.Reply(context.Background(), rctx, "可用人设: "+available)
			return
		}
		if mapped, ok := validPersonas[name]; ok {
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
		} else {
			available := "贴吧老哥/tieba, 猫娘/neko, 老干部/cadre, 母狗/simp, 萌妹/cute, 直男/straight"
			p.Reply(context.Background(), rctx, "可用人设: "+available)
		}
		return
	}

	msg := &core.Message{
		SessionKey: sessionKey,
		Platform:   "qq",
		MessageID:  strconv.FormatInt(messageID, 10),
		UserID:     strconv.FormatInt(userID, 10),
		UserName:   userName,
		ChatName:   chatName,
		Content:      text,
		ExtraContent: extraContent,
		Images:       images,
		Audio:        audio,
		ModeOverride: modeOverride,
		ReplyCtx:     rctx,
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

func (p *Platform) parseMessage(payload map[string]any) (string, []core.ImageAttachment, *core.AudioAttachment) {
	var textParts []string
	var images []core.ImageAttachment
	var audio *core.AudioAttachment

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
				if url, ok := data["url"].(string); ok && url != "" {
					imgData, mime, err := downloadFile(url)
					if err != nil {
						slog.Warn("qq: download image failed", "error", err)
						continue
					}
					images = append(images, core.ImageAttachment{
						MimeType: mime,
						Data:     imgData,
					})
				}
			case "record":
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

	return strings.TrimSpace(strings.Join(textParts, "")), images, audio
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
	err = p.conn.WriteMessage(websocket.TextMessage, data)
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
		var result map[string]any
		_ = json.Unmarshal(resp.Data, &result)
		return result, nil

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
	rctx := &replyContext{messageType: "group", groupID: groupID}
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
			p.executeCronJob(j.GroupID, j.Prompt, idx)
		})
		if err != nil {
			slog.Error("qq: invalid cron expression", "cron", j.Cron, "error", err)
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

func (p *Platform) executeCronJob(groupID int64, prompt string, _ int) {
	// Route through the AI handler so content is dynamically generated
	sessionKey := fmt.Sprintf("qq:g:%d", groupID)
	rctx := &replyContext{messageType: "group", groupID: groupID}
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

func (p *Platform) makeGroupExtraContent(sessionKey string) string {
	extra := "[群聊消息]"
	if tag, ok := p.personaMap.Load(sessionKey); ok {
		extra += "[" + tag.(string) + "]" 
	} else {
		extra += "[贴吧老哥]"
	}
	return extra
}

// ── Helpers ──

type replyContext struct {
	messageType string // "private" or "group"
	userID      int64
	groupID     int64
	messageID   int32
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// qq:{userID}, qq:{groupID}:{userID} or qq:g:{groupID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "qq" {
		return nil, fmt.Errorf("qq: invalid session key %q", sessionKey)
	}
	if len(parts) == 3 {
		if parts[1] == "g" {
			gid, _ := strconv.ParseInt(parts[2], 10, 64)
			return &replyContext{messageType: "group", groupID: gid}, nil
		}
		gid, _ := strconv.ParseInt(parts[1], 10, 64)
		uid, _ := strconv.ParseInt(parts[2], 10, 64)
		return &replyContext{messageType: "group", groupID: gid, userID: uid}, nil
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

// isPermissionApproval checks if the text is a permission approval keyword.
func isPermissionApproval(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	for _, w := range []string{
		"允许", "允许所有", "允许全部", "全部允许", "所有允许", "都允许", "全部同意",
		"同意", "可以", "好", "好的", "是", "确认",
		"allow", "allow all", "allowall", "approve", "approve all", "yes", "yes all", "y", "ok",
	} {
		if s == w {
			return true
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
