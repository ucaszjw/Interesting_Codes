// Package llm is a small OpenAI-compatible chat client for DeepSeek, including
// tool calling, the Search/Fetch tools and Summarize for context compression.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Image is an inline image attachment.
type Image struct {
	MimeType string
	Data     []byte
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON
}

// Message is one chat turn.
type Message struct {
	Role       string // "system" | "user" | "assistant" | "tool"
	Content    string
	Images     []Image
	ToolCalls  []ToolCall // assistant only
	ToolCallID string     // tool result only
}

// Tool describes a callable function.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Usage reports token accounting, including cached prompt tokens.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
}

// Response is a completed chat request.
type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
}

// Config configures the client.
type Config struct {
	BaseURL string // default https://api.deepseek.com/v1
	APIKey  string
	Model   string // default deepseek-flash
	Timeout time.Duration
}

// Client talks to the OpenAI-compatible API.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// New creates a client.
func New(cfg Config) *Client {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}
	model := cfg.Model
	if model == "" {
		model = "deepseek-flash"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{BaseURL: base, APIKey: cfg.APIKey, Model: model,
		HTTP: &http.Client{Timeout: timeout}}
}

type wireContent struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *wireImgURL `json:"image_url,omitempty"`
}

type wireImgURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Chat sends a request without tools.
func (c *Client) Chat(ctx context.Context, system string, msgs []Message) (*Response, error) {
	return c.ChatWithTools(ctx, system, msgs, nil)
}

// ChatWithTools sends one request and returns text and/or requested tool calls.
func (c *Client) ChatWithTools(ctx context.Context, system string, msgs []Message, tools []Tool) (*Response, error) {
	req := wireRequest{Model: c.Model, Tools: tools}
	if system != "" {
		req.Messages = append(req.Messages, wireMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		wm := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		switch {
		case len(m.Images) > 0:
			parts := []wireContent{{Type: "text", Text: m.Content}}
			for _, img := range m.Images {
				mime := img.MimeType
				if mime == "" {
					mime = "image/png"
				}
				parts = append(parts, wireContent{Type: "image_url", ImageURL: &wireImgURL{
					URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}})
			}
			wm.Content = parts
		default:
			wm.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			wtc := wireToolCall{ID: tc.ID, Type: "function"}
			wtc.Function.Name = tc.Name
			wtc.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, wtc)
		}
		req.Messages = append(req.Messages, wm)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm API %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var out wireResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("llm: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices")
	}
	res := &Response{
		Content: out.Choices[0].Message.Content,
		Usage: Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
			CachedTokens: out.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	for _, tc := range out.Choices[0].Message.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return res, nil
}

const describeSystem = `你是表情包标注器。先看图上有没有明显的文字：
- 有文字：直接输出图上的文字，太长就概括成关键短语，网感梗保持原样；
- 没有文字：用 2-6 个中文词描述情绪、动作或画面。
只输出标签本身，不要解释、不要标点、不要引号。例如：
狗头大笑
猫咪流泪
你礼貌吗
无语翻白眼`

// DescribeImage returns a short Chinese label for a sticker.
func (c *Client) DescribeImage(ctx context.Context, mime string, data []byte) (string, error) {
	resp, err := c.Chat(ctx, describeSystem, []Message{{
		Role: "user", Content: "给这个表情打个标签", Images: []Image{{MimeType: mime, Data: data}},
	}})
	if err != nil {
		return "", err
	}
	label := sanitizeLabel(resp.Content)
	if label == "" {
		return "", fmt.Errorf("describe: empty label")
	}
	return label, nil
}

func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"'“”‘’「」『』（）()[]【】")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	// Drop punctuation and spaces, keep a compact tag.
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '.', ',', '。', '，', '、', '!', '！', '?', '？', ':', '：', ';', '；':
			continue
		}
		b.WriteRune(r)
	}
	r := []rune(b.String())
	if len(r) > 20 {
		r = r[:20]
	}
	return string(r)
}

const verifySystem = `你是图片核对器。用户会给一张图片和一句期望内容，判断图片是否真的展示了该内容。
只输出一个 JSON 对象，不要 Markdown、不要解释：
{"match": true 或 false, "desc": "用不超过 20 个字描述图片实际内容"}`

// VerifyImage checks whether the image matches the expectation.
func (c *Client) VerifyImage(ctx context.Context, mime string, data []byte, expect string) (bool, string, error) {
	expect = strings.TrimSpace(expect)
	if expect == "" {
		return true, "", nil
	}
	prompt := fmt.Sprintf("这张图应该展示：%s\n请判断图片内容是否相符。", expect)
	resp, err := c.Chat(ctx, verifySystem, []Message{{
		Role: "user", Content: prompt, Images: []Image{{MimeType: mime, Data: data}},
	}})
	if err != nil {
		return false, "", err
	}
	body := resp.Content
	// Tolerate code fences or stray prose around the JSON object.
	if i, j := strings.Index(body, "{"), strings.LastIndex(body, "}"); i >= 0 && j > i {
		body = body[i : j+1]
	}
	var out struct {
		Match bool   `json:"match"`
		Desc  string `json:"desc"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return false, "", fmt.Errorf("verify: decode reply: %w", err)
	}
	return out.Match, out.Desc, nil
}

const summarizerSystem = `你是一个群聊记录压缩器。把用户给出的旧记录合并进已有摘要，输出一段不超过字数限制的摘要。
要求：
- 保留人物关系、正在聊的话题、未完成的约定或任务、反复出现的梗；
- 丢弃寒暄、重复内容、已被否定的信息；
- 图片和搜索结果只保留文字结论；
- 不要编造，不要评论，不要输出标题以外的格式。`

// Summarize implements chat.Summarizer.
func (c *Client) Summarize(ctx context.Context, prompt string) (string, error) {
	resp, err := c.Chat(ctx, summarizerSystem, []Message{{Role: "user", Content: prompt}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
