package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatRequestAndUsage(t *testing.T) {
	var got wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
			t.Errorf("auth = %q", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{
          "choices":[{"message":{"content":"你好"}}],
          "usage":{"prompt_tokens":120,"completion_tokens":8,
                   "prompt_tokens_details":{"cached_tokens":100}}
        }`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "key", Model: "m"})
	resp, err := c.Chat(context.Background(), "SYS", []Message{
		{Role: "user", Content: "在吗", Images: []Image{{MimeType: "image/png", Data: []byte("png")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "你好" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 8 || resp.Usage.CachedTokens != 100 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got.Model != "m" || len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Fatalf("request = %+v", got)
	}
	parts, ok := got.Messages[1].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("image message content = %#v", got.Messages[1].Content)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("image url = %q", img)
	}
}

func TestChatAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "key"})
	if _, err := c.Chat(context.Background(), "", []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSummarizeUsesSystemPrompt(t *testing.T) {
	var got wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" 摘要 "}}]}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k"})
	out, err := c.Summarize(context.Background(), "记录")
	if err != nil {
		t.Fatal(err)
	}
	if out != "摘要" {
		t.Errorf("summary = %q", out)
	}
	if len(got.Messages) != 2 || !strings.Contains(got.Messages[0].Content.(string), "压缩") {
		t.Errorf("summarizer system missing: %+v", got.Messages)
	}
}

func TestSearchAndFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tools/search":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["query"] != "golang" {
				t.Errorf("query = %v", body["query"])
			}
			w.Write([]byte(`{"results":[{"title":"T","url":"https://x","summary":"S","content":"C"}]}`))
		case "/tools/fetch":
			w.Write([]byte(`{"title":"Page","content":"# Body"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k"})
	results, err := c.Search(context.Background(), "golang", 3, true)
	if err != nil || len(results) != 1 || results[0].Title != "T" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	title, content, err := c.Fetch(context.Background(), "https://x")
	if err != nil || title != "Page" || content != "# Body" {
		t.Fatalf("fetch=%q,%q err=%v", title, content, err)
	}
}

func TestChatWithToolsParsesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[
          {"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]}}]}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "k"})
	resp, err := c.ChatWithTools(context.Background(), "s", []Message{{Role: "user", Content: "hi"}}, []Tool{{
		Type: "function", Function: ToolFunction{Name: "web_search", Parameters: json.RawMessage(`{}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "web_search" || resp.ToolCalls[0].ID != "c1" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}
