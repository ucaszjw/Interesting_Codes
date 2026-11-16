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

type wireImgURL struct{ URL string `json:"url"` }

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
