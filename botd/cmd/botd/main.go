// botd is a small QQ chat harness: NapCat OneBot transport + direct LLM calls,
// with web search/fetch tools, voice replies and scheduled prompts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/robfig/cron/v3"

	"botd/internal/chat"
	"botd/internal/core"
	"botd/internal/llm"
	"botd/internal/media"
	"botd/internal/qq"
	"botd/internal/stickers"
)

type Config struct {
	QQ struct {
		WSURL             string `toml:"ws_url"`
		Token             string `toml:"token"`
		AllowFrom         string `toml:"allow_from"`
		AdminIDs          string `toml:"admin_ids"`
		GroupAllow        string `toml:"group_allow"`
		RequireAt         bool   `toml:"require_at"`
		ShareSession      bool   `toml:"share_session_in_channel"`
		BotPeers          string `toml:"bot_peers"`
		ModerationGroups  string `toml:"moderation_groups"`
		ModerationMuteMax int    `toml:"moderation_mute_max_minutes"`
		DictatorMode      bool   `toml:"dictator_mode"`
		DarkMode          bool   `toml:"dark_mode"`
		VoiceProbability  int    `toml:"voice_probability"`
		ReplyCooldown     int    `toml:"reply_cooldown"`
		ReplyProbability  int    `toml:"reply_probability"`
		ReplyProbMin      int    `toml:"reply_prob_min"`
		ReplyProbMax      int    `toml:"reply_prob_max"`
		ReplyColdBoost    int    `toml:"reply_cold_boost"`
		ReplySkipPenalty  int    `toml:"reply_skip_penalty"`
		ReplyWithQuote    bool   `toml:"reply_with_quote"`
		EmojiLike         bool   `toml:"emoji_like"`
		CheckMute         bool   `toml:"check_mute"`
	} `toml:"qq"`

	LLM struct {
		BaseURL string `toml:"base_url"`
		APIKey  string `toml:"api_key"`
		Model   string `toml:"model"`
		Timeout int    `toml:"timeout_s"`
	} `toml:"llm"`

	TTS struct {
		Enabled     bool   `toml:"enabled"`
		Voice       string `toml:"voice"`
		Probability int    `toml:"probability"`
		MaxChars    int    `toml:"max_chars"`
		BaseURL     string `toml:"base_url"`
		APIKey      string `toml:"api_key"`
	} `toml:"tts"`

	Chat struct {
		Persona            string `toml:"persona"`
		PromptsDir         string `toml:"prompts_dir"`
		SessionsDir        string `toml:"sessions_dir"`
		HistoryMaxTokens   int    `toml:"history_max_tokens"`
		KeepRecentMessages int    `toml:"keep_recent_messages"`
		SummaryMaxChars    int    `toml:"summary_max_chars"`
	} `toml:"chat"`

	Stickers struct {
		Enabled   bool   `toml:"enabled"`
		Dir       string `toml:"dir"`
		Max       int    `toml:"max"`
		AutoLabel bool   `toml:"auto_label"`
	} `toml:"stickers"`

	Cron []CronJob `toml:"cron"`
}

// CronJob either sends a prompt through the model or a literal message
// (optionally mentioning at).
type CronJob struct {
	Cron    string `toml:"cron"`
	Prompt  string `toml:"prompt"`
	Message string `toml:"message"`
	At      int64  `toml:"at"`
	GroupID int64  `toml:"group_id"`
	Persona string `toml:"persona"`
}

type bot struct {
	cfg        Config
	prompts    *chat.Prompts
	store      *chat.Store
	client     *llm.Client
	compressor *chat.Compressor
	tts        *media.TTS
	images     *media.ImageFetcher
	stickers   *stickers.Store
	mood       *moodTracker
	tools      []llm.Tool

	// Recent inbound pictures per session, so edit_image can refer back to
	// "the last photo" without the model having to supply a URL.
	imgMu        sync.Mutex
	recentImages map[string][]cachedImage
}

type cachedImage struct {
	mime string
	data []byte
}

var toolDefs = []llm.Tool{
	{Type: "function", Function: llm.ToolFunction{
		Name:        "web_search",
		Description: "搜索网页，返回标题、链接和网页摘要。用于查最新事实、新闻、数据、版本号等拿不准的信息。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"搜索词，尽量简短直白"}},"required":["query"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "web_fetch",
		Description: "抓取一个网页并返回 Markdown 正文。需要读完整文章时使用。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"http(s) 网页地址"}},"required":["url"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "image_search",
		Description: "直接搜索图片，返回可下载的原图直链（Bing+Baidu）。找图优先用它，比翻网页可靠；结果里的 referer 是给防盗链站点用的。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"图片搜索词"}},"required":["query"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "web_images",
		Description: "列出一个网页里的图片直链。想发图但只找到图片所在的页面时，先用它拿到可直接下载的图片 URL。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"网页地址"}},"required":["url"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "send_image",
		Description: "把一张网络图片发送到当前聊天。发送前会用视觉模型核对 expect 与图片内容，不符就不发；受防盗链保护的站点可传 referer。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
          "url":{"type":"string","description":"图片的 http(s) 直链"},
          "expect":{"type":"string","description":"必填：这张图应该展示什么，用于发送前核对"},
          "referer":{"type":"string","description":"可选，防盗链站点需要的来源页，例如 https://www.pixiv.net/"}
        },"required":["url","expect"]}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name:        "send_sticker",
		Description: "从群友表情包库里发一张表情。可选 query 按表情标签（如「呲牙」）筛选，不传则随机。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"可选：表情标签关键词"}}}`),
	}},
	{Type: "function", Function: llm.ToolFunction{
		Name: "edit_image",
		Description: "裁剪/放大/锐化一张图片，然后把结果发到当前聊天。" +
			"source 默认 \"last\"，指本会话最近收到的一张图；\"last:2\" 指倒数第二张；也可传 http(s) 图片直链。" +
			"crop 用 0~1 的比例框 [x0,y0,x1,y1]；scale 放大 1~4 倍（lanczos）；sharpen 轻微锐化。" +
			"这只是普通图像处理，不是 AI 超分，别承诺无损修复。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
          "source":{"type":"string","description":"可选，默认 last；last、last:N 或 http(s) 图片直链"},
          "crop":{"type":"array","items":{"type":"number"},"minItems":4,"maxItems":4,"description":"可选，[x0,y0,x1,y1]，取值 0~1，相对原图"},
          "scale":{"type":"number","description":"可选，放大倍数 1~4"},
          "sharpen":{"type":"boolean","description":"可选，是否锐化"},
          "format":{"type":"string","enum":["jpeg","png"],"description":"可选，输出格式，默认 jpeg"},
          "expect":{"type":"string","description":"可选，发送前用视觉模型核对内容"}
        }}`),
	}},
}

func main() {
	configPath := flag.String("config", "config.toml", "path to config.toml")
	relabel := flag.Bool("relabel-stickers", false, "re-tag every stored sticker with the vision model, then exit")
	flag.Parse()

	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}
	if cfg.LLM.APIKey == "" {
		slog.Error("llm.api_key is required")
		os.Exit(1)
	}

	prompts, err := chat.LoadPrompts(cfg.Chat.PromptsDir)
	if err != nil {
		slog.Error("prompts", "error", err)
		os.Exit(1)
	}
	store, err := chat.NewStore(cfg.Chat.SessionsDir)
	if err != nil {
		slog.Error("sessions", "error", err)
		os.Exit(1)
	}
	client := llm.New(llm.Config{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
		Timeout: time.Duration(cfg.LLM.Timeout) * time.Second,
	})

	moodPath := ""
	if d := strings.TrimSpace(cfg.Chat.SessionsDir); d != "" {
		moodPath = filepath.Join(filepath.Dir(d), "mood.json")
	}
	b := &bot{
		cfg:     cfg,
		prompts: prompts,
		store:   store,
		client:  client,
		images:  media.NewImageFetcher(false),
		mood:    newMoodTracker(moodPath),
		tools:   toolDefs,
	}
	if cfg.Chat.HistoryMaxTokens > 0 {
		b.compressor = &chat.Compressor{
			Summarizer:      client,
			MaxTokens:       cfg.Chat.HistoryMaxTokens,
			KeepRecent:      cfg.Chat.KeepRecentMessages,
			SummaryMaxChars: cfg.Chat.SummaryMaxChars,
		}
	}
	if cfg.TTS.Enabled {
		key := cfg.TTS.APIKey
		if key == "" {
			key = cfg.LLM.APIKey
		}
		base := cfg.TTS.BaseURL
		if base == "" {
			base = cfg.LLM.BaseURL
		}
		b.tts = media.NewTTS(key, base, cfg.TTS.Voice)
	}
	if cfg.Stickers.Enabled {
		dir := cfg.Stickers.Dir
		if dir == "" {
			dir = "data/stickers"
		}
		st, err := stickers.NewStore(dir, cfg.Stickers.Max)
		if err != nil {
			slog.Error("stickers", "error", err)
			os.Exit(1)
		}
		b.stickers = st
		slog.Info("stickers loaded", "count", st.Count(), "dir", dir)
	}
	if *relabel {
		if b.stickers == nil {
			slog.Error("relabel-stickers: sticker library is disabled")
			os.Exit(1)
		}
		b.relabelAllStickers()
		return
	}

	dataDir := ""
	if d := strings.TrimSpace(cfg.Chat.SessionsDir); d != "" {
		dataDir = filepath.Dir(d)
	}
	opts := map[string]any{
		"cc_data_dir":                 dataDir,
		"cc_project":                  "botd",
		"ws_url":                      cfg.QQ.WSURL,
		"token":                       cfg.QQ.Token,
		"allow_from":                  cfg.QQ.AllowFrom,
		"admin_ids":                   cfg.QQ.AdminIDs,
		"group_allow":                 cfg.QQ.GroupAllow,
		"require_at":                  cfg.QQ.RequireAt,
		"share_session_in_channel":    cfg.QQ.ShareSession,
		"bot_peers":                   cfg.QQ.BotPeers,
		"moderation_groups":           cfg.QQ.ModerationGroups,
		"moderation_mute_max_minutes": cfg.QQ.ModerationMuteMax,
		"dictator_mode":               cfg.QQ.DictatorMode,
		"dark_mode":                   cfg.QQ.DarkMode,
		"voice_probability":           cfg.QQ.VoiceProbability,
		"reply_cooldown":              cfg.QQ.ReplyCooldown,
		"reply_probability":           cfg.QQ.ReplyProbability,
		"reply_prob_min":              cfg.QQ.ReplyProbMin,
		"reply_prob_max":              cfg.QQ.ReplyProbMax,
		"reply_cold_boost":            cfg.QQ.ReplyColdBoost,
		"reply_skip_penalty":          cfg.QQ.ReplySkipPenalty,
		"reply_with_quote":            cfg.QQ.ReplyWithQuote,
		"emoji_like":                  cfg.QQ.EmojiLike,
		"check_mute":                  cfg.QQ.CheckMute,
		// Private chats are the operator's workspace; non-admin DMs are dropped
		// by the transport before they reach the model.
		"tool_admin_only": true,
	}
	platform, err := qq.New(opts)
	if err != nil {
		slog.Error("qq platform", "error", err)
		os.Exit(1)
	}
	if err := platform.Start(b.handle); err != nil {
		slog.Error("start", "error", err)
		os.Exit(1)
	}
	scheduler := b.startCron(platform, cfg.Cron)
	slog.Info("botd is running", "project", cfg.Chat.Persona, "tts", cfg.TTS.Enabled, "cron_jobs", len(cfg.Cron))
	if cfg.Stickers.AutoLabel && b.stickers != nil {
		go b.backfillStickerLabels()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	if scheduler != nil {
		scheduler.Stop()
	}
	_ = platform.Stop()
}

// handle is called for every message the transport accepted (allowlist, scope
// and reply throttle already applied).
func (b *bot) handle(p core.Platform, msg *core.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sess, err := b.store.Load(msg.SessionKey)
	if err != nil {
		slog.Error("session load", "error", err)
		return
	}
	if sess.Persona == "" {
		sess.Persona = b.cfg.Chat.Persona
		if sess.Persona == "" {
			sess.Persona = "大肥鱼"
		}
	}
	// Facts belong to the chat, not to one thread, so they survive /new.
	if facts := b.store.LoadFacts(msg.SessionKey); len(facts) > 0 {
		sess.Facts = facts
	}
	// /dict and /dark change the rules; continuing in the old conversation made
	// the model apply them to stale context, so start a fresh thread.
	if rs, ok := p.(core.ModeResetter); ok && rs.ConsumeModeReset(msg.SessionKey) {
		ns, err := b.store.NewSession(msg.SessionKey, "")
		if err != nil {
			slog.Error("mode switch: new session", "error", err)
		} else {
			ns.Persona = sess.Persona
			ns.Facts = sess.Facts
			_ = b.store.Save(ns)
			sess = ns
			slog.Info("mode switch: started new session", "session", msg.SessionKey, "id", ns.ID)
		}
	}
	// Learn memes from the group: sticker images are kept in a local library.
	if b.stickers != nil && isGroupSession(msg.SessionKey) {
		for _, img := range msg.Images {
			if !img.Sticker {
				continue
			}
			id, added, err := b.stickers.Add(img.MimeType, img.Data, img.Label)
			if err != nil {
				slog.Warn("sticker add failed", "error", err)
			} else if added {
				slog.Info("sticker saved", "label", img.Label, "count", b.stickers.Count())
				if b.cfg.Stickers.AutoLabel && needsAutoLabel(img.Label) {
					// Async so the chat turn is not delayed by the vision call.
					go b.autoLabelSticker(id, img)
				}
			}
		}
	}

	b.rememberImages(msg.SessionKey, msg.Images)

	text := strings.TrimSpace(msg.Content)
	if cmd, ok := strings.CutPrefix(text, "/"); ok {
		if b.handleCommand(ctx, p, msg, sess, cmd) {
			return
		}
	}

	system := b.prompts.System(groupKey(msg.SessionKey), sess.Persona)

	var turns []llm.Message
	if mem := sess.MemoryPrompt(); mem != "" {
		turns = append(turns, llm.Message{Role: "system", Content: mem})
	}
	for _, m := range sess.Messages {
		content := m.Content
		if m.Role == "user" {
			content = fmt.Sprintf("%s(%s): %s", m.Name, m.UserID, content)
			if m.Images > 0 {
				content += " [图片]"
			}
		}
		turns = append(turns, llm.Message{Role: m.Role, Content: content})
	}

	current := fmt.Sprintf("%s(%s): %s", msg.UserName, msg.UserID, text)
	// The transport knows whether the bot is owner/admin in this group; surface
	// it the same way the prompt expects ([群管:群主] at the message start).
	if rh, ok := p.(interface{ RoleHint(string) string }); ok {
		if hint := rh.RoleHint(msg.SessionKey); hint != "" {
			if hint == "[群管:独裁]" {
				// Dictator mode judges requests by the group's recent atmosphere:
				// praise lifts the mood, insults sour it.
				mood := "一般"
				if gid, ok := groupIDOf(msg.SessionKey); ok && b.mood != nil {
					mood = b.mood.Current(gid)
				}
				hint += " [心情:" + mood + "]"
			}
			current = hint + " " + current
		}
	}
	var images []llm.Image
	for _, img := range msg.Images {
		images = append(images, llm.Image{MimeType: img.MimeType, Data: img.Data})
	}
	turns = append(turns, llm.Message{Role: "user", Content: current, Images: images})

	turnStart := time.Now()
	reply, usage, err := b.generate(ctx, system, turns, p, msg.ReplyCtx, msg.SessionKey)
	if err != nil {
		slog.Error("chat", "error", err, "session", msg.SessionKey)
		_ = p.Reply(ctx, msg.ReplyCtx, "刚才走神了，再说一遍？")
		return
	}
	slog.Info("chat turn",
		"session", msg.SessionKey, "persona", sess.Persona,
		"input_tokens", usage.InputTokens,
		"cached_tokens", usage.CachedTokens,
		"output_tokens", usage.OutputTokens,
		"duration", time.Since(turnStart).Round(time.Millisecond))
	// The model decides its own mood through a hidden marker; strip it before
	// anything is stored, spoken or sent.
	reply, moodDelta := parseMoodMarker(reply)
	if gid, ok := groupIDOf(msg.SessionKey); ok && b.mood != nil {
		b.mood.Apply(gid, moodDelta)
	}
	if reply == "" || strings.EqualFold(reply, "NO_REPLY") {
		return
	}

	if !b.sendReply(ctx, p, msg, reply) {
		slog.Error("send failed", "session", msg.SessionKey)
	}

	now := time.Now()
	sess.Messages = append(sess.Messages,
		chat.Msg{Role: "user", Name: msg.UserName, UserID: msg.UserID, Content: text, Images: len(msg.Images), At: now},
		chat.Msg{Role: "assistant", Content: reply, At: now},
	)
	sess.LastPromptTokens = usage.InputTokens
	if b.compressor != nil {
		if changed, cerr := b.compressor.Maybe(context.Background(), sess); cerr != nil {
			slog.Warn("compress", "error", cerr, "session", msg.SessionKey)
		} else if changed {
			slog.Info("compress: summarized old turns", "session", msg.SessionKey)
		}
	}
	if err := b.store.Save(sess); err != nil {
		slog.Error("session save", "error", err)
	}
}

// generate runs the tool-calling loop until the model produces plain text.
func (b *bot) generate(ctx context.Context, system string, turns []llm.Message, p core.Platform, replyCtx any, sessionKey string) (string, llm.Usage, error) {
	const maxRounds = 8
	var usage llm.Usage
	sentAttachment := false
	for round := 0; round <= maxRounds; round++ {
		resp, err := b.client.ChatWithTools(ctx, system, turns, b.tools)
		if err != nil {
			return "", usage, err
		}
		usage = resp.Usage
		if len(resp.ToolCalls) == 0 {
			return strings.TrimSpace(resp.Content), usage, nil
		}
		turns = append(turns, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			out := b.runTool(ctx, tc, p, replyCtx, sessionKey)
			if strings.HasPrefix(out, "图片已发送") || strings.HasPrefix(out, "已发送表情") {
				sentAttachment = true
			}
			slog.Info("tool call", "name", tc.Name,
				"args", truncateRunes(tc.Arguments, 120),
				"result", truncateRunes(out, 120))
			turns = append(turns, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
	if sentAttachment {
		// The images already went out; a short confirmation beats the generic
		// "something went wrong" fallback.
		return "图发你了 🐋", usage, nil
	}
	return "", usage, fmt.Errorf("tool loop exceeded %d rounds", maxRounds)
}

func (b *bot) runTool(ctx context.Context, tc llm.ToolCall, p core.Platform, replyCtx any, sessionKey string) string {
	switch tc.Name {
	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return "参数错误：需要 query"
		}
		results, err := b.client.Search(ctx, args.Query, 8, true)
		if err != nil {
			return "搜索失败：" + err.Error()
		}
		if len(results) == 0 {
			return "没有搜到结果"
		}
		var sb strings.Builder
		for i, r := range results {
			body := r.Content
			if body == "" {
				body = r.Summary
			}
			if body == "" {
				body = r.Snippet
			}
			fmt.Fprintf(&sb, "[%d] %s\n%s\n%s\n\n", i+1, r.Title, r.URL, truncateRunes(body, 1200))
		}
		return truncateRunes(sb.String(), 6000)
	case "web_fetch":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.URL) == "" {
			return "参数错误：需要 url"
		}
		title, content, err := b.client.Fetch(ctx, args.URL)
		if err != nil {
			return "抓取失败：" + err.Error()
		}
		return truncateRunes("# "+title+"\n\n"+content, 6000)
	case "image_search":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return "参数错误：需要 query"
		}
		results, err := b.images.SearchImages(ctx, args.Query, 10)
		if err != nil {
			return "搜图失败：" + err.Error()
		}
		var sb strings.Builder
		for i, r := range results {
			title := r.Title
			if title == "" {
				title = "图片"
			}
			fmt.Fprintf(&sb, "%d. %s\n   %s\n   referer: %s\n", i+1, title, r.URL, r.Referer)
		}
		return truncateRunes(sb.String(), 4000)
	case "web_images":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.URL) == "" {
			return "参数错误：需要 url"
		}
		finalURL, body, err := b.images.FetchHTML(ctx, args.URL)
		if err != nil {
			return "抓取网页失败：" + err.Error()
		}
		urls := media.ExtractImageURLs(finalURL, body, 10)
		if len(urls) == 0 {
			return "这个页面里没有找到图片直链"
		}
		var sb strings.Builder
		for i, u := range urls {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, u)
		}
		return "图片直链：\n" + strings.TrimSpace(sb.String())
	case "send_image":
		var args struct {
			URL     string `json:"url"`
			Expect  string `json:"expect"`
			Referer string `json:"referer"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.URL) == "" {
			return "参数错误：需要 url"
		}
		if strings.TrimSpace(args.Expect) == "" {
			return "参数错误：需要 expect（描述这张图应该展示什么）"
		}
		mime, data, err := b.images.Fetch(ctx, args.URL, strings.TrimSpace(args.Referer))
		if err != nil {
			return "发图失败：" + err.Error()
		}
		// Check the picture before sending it. Send a small thumbnail so a 1.5MB
		// original does not turn into a huge verification request.
		verifyMime, verifyData := mime, data
		if thumb, terr := media.ThumbnailJPEG(ctx, data, 512); terr == nil {
			verifyMime, verifyData = "image/jpeg", thumb
		}
		match, desc, verr := b.client.VerifyImage(ctx, verifyMime, verifyData, args.Expect)
		if verr != nil {
			slog.Warn("image verify failed, sending anyway", "error", verr)
		} else if !match {
			slog.Info("image rejected by verifier", "expect", args.Expect, "actual", desc)
			return "图片内容不符，未发送：" + desc
		} else {
			slog.Info("image verified", "expect", args.Expect, "actual", desc)
		}
		sender, ok := p.(core.ImageSender)
		if !ok {
			return "当前平台不支持发图"
		}
		if err := sender.SendImage(ctx, replyCtx, core.ImageAttachment{MimeType: mime, Data: data}); err != nil {
			return "发图失败：" + err.Error()
		}
		slog.Info("image sent", "url", args.URL, "mime", mime, "bytes", len(data))
		return "图片已发送"
	case "send_sticker":
		if b.stickers == nil || b.stickers.Count() == 0 {
			return "表情库还是空的"
		}
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal([]byte(tc.Arguments), &args)
		entry, data, ok := b.stickers.Find(args.Query)
		if !ok {
			return "没有可用的表情"
		}
		sender, ok := p.(core.ImageSender)
		if !ok {
			return "当前平台不支持发图"
		}
		if err := sender.SendImage(ctx, replyCtx, core.ImageAttachment{MimeType: entry.Mime, Data: data}); err != nil {
			return "发表情失败：" + err.Error()
		}
		label := entry.Label
		if label == "" {
			label = entry.ID
		}
		slog.Info("sticker sent", "label", entry.Label, "count", b.stickers.Count())
		return "已发送表情 " + label
	case "edit_image":
		var args struct {
			Source  string    `json:"source"`
			Crop    []float64 `json:"crop"`
			Scale   float64   `json:"scale"`
			Sharpen bool      `json:"sharpen"`
			Format  string    `json:"format"`
			Expect  string    `json:"expect"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return "参数错误：" + err.Error()
		}
		source := strings.TrimSpace(args.Source)
		if source == "" {
			source = "last"
		}
		var mime string
		var data []byte
		switch {
		case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
			var ferr error
			mime, data, ferr = b.images.Fetch(ctx, source, "")
			if ferr != nil {
				return "取图失败：" + ferr.Error()
			}
		case source == "last" || strings.HasPrefix(source, "last:"):
			n := 1
			if i := strings.Index(source, ":"); i >= 0 {
				v, perr := strconv.Atoi(source[i+1:])
				if perr != nil || v < 1 {
					return "参数错误：source 应为 last、last:N 或图片链接"
				}
				n = v
			}
			ci, ok := b.recentImage(sessionKey, n)
			if !ok {
				return "没找到最近的图片；让群里先发一张，或直接给图片直链"
			}
			mime, data = ci.mime, ci.data
		default:
			return "参数错误：source 应为 last、last:N 或图片链接"
		}
		var crop *[4]float64
		if args.Crop != nil {
			if len(args.Crop) != 4 {
				return "参数错误：crop 需要 4 个 0~1 的数"
			}
			box := [4]float64{args.Crop[0], args.Crop[1], args.Crop[2], args.Crop[3]}
			crop = &box
		}
		// Keep PNG/GIF sources as PNG so transparent stickers don't get a
		// black background from JPEG encoding.
		format := strings.ToLower(strings.TrimSpace(args.Format))
		if format == "" && (strings.Contains(mime, "png") || strings.Contains(mime, "gif")) {
			format = "png"
		}
		outMime, outData, err := media.EditImage(ctx, data, media.EditOptions{
			Crop: crop, Scale: args.Scale, Sharpen: args.Sharpen, Format: format,
		})
		if err != nil {
			return "处理失败：" + err.Error()
		}
		if expect := strings.TrimSpace(args.Expect); expect != "" {
			vm, vd := outMime, outData
			if thumb, terr := media.ThumbnailJPEG(ctx, outData, 512); terr == nil {
				vm, vd = "image/jpeg", thumb
			}
			match, desc, verr := b.client.VerifyImage(ctx, vm, vd, expect)
			if verr != nil {
				slog.Warn("edited image verify failed, sending anyway", "error", verr)
			} else if !match {
				return "处理后的图对不上，未发送：" + desc
			}
		}
		sender, ok := p.(core.ImageSender)
		if !ok {
			return "当前平台不支持发图"
		}
		if err := sender.SendImage(ctx, replyCtx, core.ImageAttachment{MimeType: outMime, Data: outData}); err != nil {
			return "发图失败：" + err.Error()
		}
		w, h, _ := media.ImageSize(outData)
		slog.Info("image edited", "source", source, "mime", outMime, "bytes", len(outData), "size", fmt.Sprintf("%dx%d", w, h))
		return fmt.Sprintf("图片已发送（%dx%d）", w, h)
	}
	return "未知工具：" + tc.Name
}

// rememberImages keeps the newest inbound pictures for each session so
// edit_image can refer back to "the last photo in this chat".
func (b *bot) rememberImages(sessionKey string, imgs []core.ImageAttachment) {
	const maxPerSession = 6
	const maxBytes = 8 << 20
	if len(imgs) == 0 {
		return
	}
	b.imgMu.Lock()
	defer b.imgMu.Unlock()
	if b.recentImages == nil {
		b.recentImages = make(map[string][]cachedImage)
	}
	list := b.recentImages[sessionKey]
	for _, img := range imgs {
		if len(img.Data) == 0 || len(img.Data) > maxBytes {
			continue
		}
		mime := img.MimeType
		if mime == "" {
			mime = "image/jpeg"
		}
		list = append(list, cachedImage{mime: mime, data: img.Data})
	}
	if len(list) > maxPerSession {
		list = list[len(list)-maxPerSession:]
	}
	b.recentImages[sessionKey] = list
}

// recentImage returns the n-th newest inbound picture (1 = newest).
func (b *bot) recentImage(sessionKey string, n int) (cachedImage, bool) {
	b.imgMu.Lock()
	defer b.imgMu.Unlock()
	list := b.recentImages[sessionKey]
	if n < 1 || n > len(list) {
		return cachedImage{}, false
	}
	return list[len(list)-n], true
}

// sendReply prefers voice when eligible, falling back to text.
func (b *bot) sendReply(ctx context.Context, p core.Platform, msg *core.Message, reply string) bool {
	if b.shouldVoice(msg, reply) {
		audio, format, err := b.tts.Synthesize(ctx, reply)
		if err == nil {
			if as, ok := p.(core.AudioSender); ok {
				if err := as.SendAudio(ctx, msg.ReplyCtx, audio, format); err == nil {
					slog.Info("voice reply", "session", msg.SessionKey, "bytes", len(audio))
					return true
				}
			}
		} else {
			slog.Warn("tts failed, falling back to text", "error", err)
		}
	}
	return p.Send(ctx, msg.ReplyCtx, reply) == nil
}

// relabelAllStickers re-tags every entry, ignoring existing labels.
func (b *bot) relabelAllStickers() {
	n := 0
	for _, e := range b.stickers.Entries() {
		entry, data, ok := b.stickers.Read(e.ID)
		if !ok || len(data) == 0 {
			continue
		}
		b.autoLabelSticker(entry.ID, core.ImageAttachment{
			MimeType: entry.Mime, Data: data, Label: entry.Label,
		})
		n++
	}
	slog.Info("stickers relabeled", "count", n)
}

// backfillStickerLabels tags older entries that only carry a generic QQ
// summary. A few per boot keeps the vision API from being flooded at startup.
func (b *bot) backfillStickerLabels() {
	const perBoot = 20
	n := 0
	for _, e := range b.stickers.Entries() {
		if n >= perBoot {
			break
		}
		if !needsAutoLabel(e.Label) {
			continue
		}
		entry, data, ok := b.stickers.Read(e.ID)
		if !ok || len(data) == 0 {
			continue
		}
		b.autoLabelSticker(entry.ID, core.ImageAttachment{
			MimeType: entry.Mime, Data: data, Label: entry.Label,
		})
		n++
	}
	if n > 0 {
		slog.Info("sticker labels backfilled", "count", n)
	}
}

// autoLabelSticker asks the vision model for a short tag and updates the entry.
func (b *bot) autoLabelSticker(id string, img core.ImageAttachment) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	mime, data := img.MimeType, img.Data
	if thumb, err := media.ThumbnailJPEG(ctx, data, 512); err == nil {
		mime, data = "image/jpeg", thumb
	}
	label, err := b.client.DescribeImage(ctx, mime, data)
	if err != nil {
		slog.Warn("sticker label failed", "error", err)
		return
	}
	if b.stickers.SetLabel(id, label) {
		slog.Info("sticker labeled", "id", id, "label", label)
	}
}

// needsAutoLabel reports whether the QQ summary is too generic to search by.
func needsAutoLabel(label string) bool {
	switch strings.TrimSpace(label) {
	case "", "[动画表情]", "[表情]", "[图片]":
		return true
	}
	return false
}

func (b *bot) shouldVoice(msg *core.Message, reply string) bool {
	if b.tts == nil || !b.cfg.TTS.Enabled || !isGroupSession(msg.SessionKey) {
		return false
	}
	// Markers trigger actions and only run on the text path.
	if qq.HasActionMarkers(reply) {
		return false
	}
	if b.cfg.TTS.MaxChars > 0 && len([]rune(reply)) > b.cfg.TTS.MaxChars {
		return false
	}
	if msg.FromVoice {
		return true
	}
	p := b.cfg.TTS.Probability
	if p <= 0 {
		return false
	}
	if p >= 100 {
		return true
	}
	return rand.Intn(100) < p
}

// handleCommand returns true when the message was a command and was answered.
func (b *bot) handleCommand(ctx context.Context, p core.Platform, msg *core.Message, sess *chat.Session, cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "new":
		ns, err := b.store.NewSession(msg.SessionKey, "")
		if err != nil {
			_ = p.Reply(ctx, msg.ReplyCtx, "开会话失败："+err.Error())
			return true
		}
		ns.Persona = sess.Persona
		ns.Facts = sess.Facts
		_ = b.store.Save(ns)
		_ = p.Reply(ctx, msg.ReplyCtx,
			fmt.Sprintf("新会话 %s 开好了 🐋（/list 查看，/switch 切换）", ns.ID))
		return true
	case "reset":
		b.store.Reset(sess)
		_ = b.store.Save(sess)
		_ = p.Reply(ctx, msg.ReplyCtx, "当前会话已清空 🐋")
		return true
	case "list", "sessions":
		infos := b.store.List(msg.SessionKey)
		var sb strings.Builder
		for i, info := range infos {
			mark := "  "
			if info.ID == sess.ID {
				mark = "▶ "
			}
			name := ""
			if info.Name != "" {
				name = "「" + info.Name + "」"
			}
			fmt.Fprintf(&sb, "%s%d. %s%s · %d 条 · %s\n",
				mark, i+1, info.ID, name, info.Messages, info.UpdatedAt.Format("01-02 15:04"))
		}
		_ = p.Reply(ctx, msg.ReplyCtx, "会话列表：\n"+strings.TrimSpace(sb.String()))
		return true
	case "switch":
		if len(fields) < 2 {
			_ = p.Reply(ctx, msg.ReplyCtx, "用法：/switch 序号|会话ID|名称")
			return true
		}
		ns, err := b.store.Switch(msg.SessionKey, strings.Join(fields[1:], " "))
		if err != nil {
			_ = p.Reply(ctx, msg.ReplyCtx, err.Error())
			return true
		}
		_ = p.Reply(ctx, msg.ReplyCtx,
			fmt.Sprintf("已切到会话 %s（%d 条消息）🐋", ns.ID, len(ns.Messages)))
		return true
	case "name":
		if len(fields) < 2 {
			_ = p.Reply(ctx, msg.ReplyCtx, "用法：/name 会话名称")
			return true
		}
		if err := b.store.Rename(sess, strings.Join(fields[1:], " ")); err != nil {
			_ = p.Reply(ctx, msg.ReplyCtx, "改名失败："+err.Error())
			return true
		}
		_ = p.Reply(ctx, msg.ReplyCtx, "改名好了 🐋")
		return true
	case "persona":
		if len(fields) < 2 {
			_ = p.Reply(ctx, msg.ReplyCtx, "可用人设："+strings.Join(b.prompts.PersonaNames(), "、"))
			return true
		}
		name := fields[1]
		if _, ok := b.prompts.Personas[name]; !ok {
			_ = p.Reply(ctx, msg.ReplyCtx, "没有这个人设："+name)
			return true
		}
		sess.Persona = name
		_ = b.store.Save(sess)
		_ = p.Reply(ctx, msg.ReplyCtx, "好，切到 "+name+" 了 🐋")
		return true
	case "emo":
		if b.stickers == nil || b.stickers.Count() == 0 {
			_ = p.Reply(ctx, msg.ReplyCtx, "表情库还是空的，等群友发表情")
			return true
		}
		entries := b.stickers.Entries()
		if len(fields) == 1 {
			const maxList = 100
			var sb strings.Builder
			fmt.Fprintf(&sb, "表情库（%d 张）：\n", len(entries))
			for i, e := range entries {
				if i >= maxList {
					fmt.Fprintf(&sb, "…共 %d 张，只显示前 %d 条\n", len(entries), maxList)
					break
				}
				label := e.Label
				if label == "" {
					label = "未标注"
				}
				fmt.Fprintf(&sb, "%d. %s\n", i+1, label)
			}
			sb.WriteString("用 /emo 序号、/emo 标签 或 /emo rand 发送")
			_ = p.Reply(ctx, msg.ReplyCtx, strings.TrimSpace(sb.String()))
			return true
		}
		query := strings.Join(fields[1:], " ")
		var hit stickers.Entry
		found := false
		switch {
		case strings.EqualFold(query, "rand") || query == "随机":
			hit, found = entries[rand.Intn(len(entries))], true
		default:
			if n, err := strconv.Atoi(query); err == nil {
				if n >= 1 && n <= len(entries) {
					hit, found = entries[n-1], true
				}
			} else {
				for _, e := range entries {
					if strings.Contains(strings.ToLower(e.Label), strings.ToLower(query)) {
						hit, found = e, true
						break
					}
				}
			}
		}
		if !found {
			_ = p.Reply(ctx, msg.ReplyCtx, "没有这个表情，发 /emo 看看清单")
			return true
		}
		entry, data, ok := b.stickers.Read(hit.ID)
		if !ok {
			_ = p.Reply(ctx, msg.ReplyCtx, "这个表情文件丢了，换一个吧")
			return true
		}
		sender, ok := p.(core.ImageSender)
		if !ok {
			_ = p.Reply(ctx, msg.ReplyCtx, "当前平台不支持发图")
			return true
		}
		if err := sender.SendImage(ctx, msg.ReplyCtx, core.ImageAttachment{MimeType: entry.Mime, Data: data}); err != nil {
			_ = p.Reply(ctx, msg.ReplyCtx, "发表情失败："+err.Error())
			return true
		}
		slog.Info("emo sent", "label", entry.Label, "id", entry.ID)
		return true
	case "facts":
		if len(sess.Facts) == 0 {
			_ = p.Reply(ctx, msg.ReplyCtx, "还没有长期记忆")
			return true
		}
		var sb strings.Builder
		for i, f := range sess.Facts {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, f)
		}
		_ = p.Reply(ctx, msg.ReplyCtx, strings.TrimSpace(sb.String()))
		return true
	case "remember":
		if !b.isAdmin(msg.UserID) {
			_ = p.Reply(ctx, msg.ReplyCtx, "只有管理员能写长期记忆")
			return true
		}
		fact := strings.TrimSpace(strings.TrimPrefix(cmd, "remember"))
		if fact == "" {
			_ = p.Reply(ctx, msg.ReplyCtx, "用法：/remember 要记住的事")
			return true
		}
		for _, f := range sess.Facts {
			if f == fact {
				_ = p.Reply(ctx, msg.ReplyCtx, "这条已经记过了")
				return true
			}
		}
		sess.Facts = append(sess.Facts, fact)
		if len(sess.Facts) > 30 {
			sess.Facts = sess.Facts[len(sess.Facts)-30:]
		}
		_ = b.store.SaveFacts(msg.SessionKey, sess.Facts)
		_ = p.Reply(ctx, msg.ReplyCtx, "记下了 🐋")
		return true
	case "forget":
		if !b.isAdmin(msg.UserID) {
			_ = p.Reply(ctx, msg.ReplyCtx, "只有管理员能改长期记忆")
			return true
		}
		arg := strings.TrimSpace(strings.TrimPrefix(cmd, "forget"))
		if arg == "" {
			_ = p.Reply(ctx, msg.ReplyCtx, "用法：/forget 序号或内容")
			return true
		}
		if n, err := parseInt(arg); err == nil && n >= 1 && n <= len(sess.Facts) {
			sess.Facts = append(sess.Facts[:n-1], sess.Facts[n:]...)
			_ = b.store.SaveFacts(msg.SessionKey, sess.Facts)
			_ = p.Reply(ctx, msg.ReplyCtx, "忘了 🐋")
			return true
		}
		kept := sess.Facts[:0]
		removed := false
		for _, f := range sess.Facts {
			if !removed && strings.Contains(f, arg) {
				removed = true
				continue
			}
			kept = append(kept, f)
		}
		sess.Facts = kept
		_ = b.store.SaveFacts(msg.SessionKey, sess.Facts)
		if removed {
			_ = p.Reply(ctx, msg.ReplyCtx, "忘了 🐋")
		} else {
			_ = p.Reply(ctx, msg.ReplyCtx, "没找到这条记忆")
		}
		return true
	}
	return false
}

func (b *bot) isAdmin(userID string) bool {
	for _, id := range strings.Split(b.cfg.QQ.AdminIDs, ",") {
		if strings.TrimSpace(id) == userID {
			return true
		}
	}
	return false
}

func (b *bot) startCron(p core.Platform, jobs []CronJob) *cron.Cron {
	if len(jobs) == 0 {
		return nil
	}
	c := cron.New()
	for _, job := range jobs {
		job := job
		if job.Cron == "" || job.GroupID == 0 || (job.Prompt == "" && job.Message == "") {
			continue
		}
		if _, err := c.AddFunc(job.Cron, func() { b.runCron(p, job) }); err != nil {
			slog.Error("cron: invalid schedule", "cron", job.Cron, "error", err)
			continue
		}
		slog.Info("cron registered", "cron", job.Cron, "group", job.GroupID, "prompt", job.Prompt)
	}
	c.Start()
	return c
}

func (b *bot) runCron(p core.Platform, job CronJob) {
	sessionKey := fmt.Sprintf("qq:g:%d", job.GroupID)
	reconstructor, ok := p.(core.ReplyContextReconstructor)
	if !ok {
		slog.Error("cron: platform cannot reconstruct reply context")
		return
	}
	replyCtx, err := reconstructor.ReconstructReplyCtx(sessionKey)
	if err != nil {
		slog.Error("cron: reply context", "error", err, "session", sessionKey)
		return
	}
	if job.Message != "" {
		content := job.Message
		if job.At != 0 {
			content = fmt.Sprintf("[at:%d] %s", job.At, content)
		}
		slog.Info("cron firing literal message", "group", job.GroupID, "message", job.Message)
		if err := p.Send(context.Background(), replyCtx, content); err != nil {
			slog.Error("cron: send", "error", err, "group", job.GroupID)
		}
		return
	}
	if job.Persona != "" {
		if sess, err := b.store.Load(sessionKey); err == nil {
			sess.Persona = job.Persona
			_ = b.store.Save(sess)
		}
	}
	slog.Info("cron firing", "group", job.GroupID, "prompt", job.Prompt)
	b.handle(p, &core.Message{
		SessionKey: sessionKey,
		UserID:     "cron",
		UserName:   "定时任务",
		Content:    job.Prompt,
		ReplyCtx:   replyCtx,
	})
}

func isGroupSession(sessionKey string) bool {
	parts := strings.Split(sessionKey, ":")
	return len(parts) == 3 && parts[0] == "qq"
}

// groupIDOf extracts the numeric group id from a qq group session key.
func groupIDOf(sessionKey string) (int64, bool) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) != 3 || parts[0] != "qq" {
		return 0, false
	}
	raw := parts[1]
	if raw == "g" {
		raw = parts[2]
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil
}

// groupKey maps a session key to the prompts/groups/<key>.md lookup.
func groupKey(sessionKey string) string {
	parts := strings.Split(sessionKey, ":")
	if len(parts) == 3 && parts[0] == "qq" {
		if parts[1] == "g" {
			return parts[2]
		}
		return parts[1]
	}
	return "private"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
