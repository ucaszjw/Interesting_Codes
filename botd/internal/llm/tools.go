package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SearchResult is one web result from the DeepSeek search tool.
type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Snippet string  `json:"snippet"`
	Summary string  `json:"summary"`
	Content string  `json:"content"`
	Score   float64 `json:"rerank_score"`
}

// Search queries DeepSeek's in-house search engine.
func (c *Client) Search(ctx context.Context, query string, count int, needContent bool) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	if count <= 0 || count > 50 {
		count = 8
	}
	body, _ := json.Marshal(map[string]any{
		"query":        query,
		"count":        count,
		"need_content": needContent,
	})
	resp, err := c.postJSON(ctx, "/tools/search", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("search: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search API %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("search: decode: %w", err)
	}
	return out.Results, nil
}

// Fetch reads one page and returns its Markdown body.
func (c *Client) Fetch(ctx context.Context, url string) (title, content string, err error) {
	body, _ := json.Marshal(map[string]any{"url": url})
	resp, err := c.postJSON(ctx, "/tools/fetch", body)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("fetch: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch API %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("fetch: decode: %w", err)
	}
	return out.Title, out.Content, nil
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: %w", path, err)
	}
	return resp, nil
}
