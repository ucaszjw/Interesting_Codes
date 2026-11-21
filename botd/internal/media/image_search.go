package media

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
)

// ImageResult is one image found by the image search tool.
type ImageResult struct {
	Title   string
	URL     string
	Referer string
}

// SearchImages queries image search engines and returns direct image links.
// Bing is tried first (its result pages embed full-size "murl"s), then Baidu,
// which handles Chinese subjects better.
func (f *ImageFetcher) SearchImages(ctx context.Context, query string, limit int) ([]ImageResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}
	if limit <= 0 {
		limit = 10
	}
	var out []ImageResult
	// Baidu is primary: its Chinese results are far more relevant. Bing is a
	// fallback because its unauthenticated image results are often generic.
	if rs := f.searchBaiduImages(ctx, query); len(rs) > 0 {
		out = append(out, rs...)
	}
	if len(out) < limit {
		if rs := f.searchBingImages(ctx, query); len(rs) > 0 {
			out = append(out, rs...)
		}
	}
	seen := map[string]bool{}
	kept := out[:0]
	for _, r := range out {
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		kept = append(kept, r)
		if len(kept) >= limit {
			break
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("没有搜到图片")
	}
	return kept, nil
}

func (f *ImageFetcher) searchBingImages(ctx context.Context, query string) []ImageResult {
	u := "https://www.bing.com/images/search?q=" + url.QueryEscape(query) +
		"&form=HDRSC2&first=1&tsc=ImageBasicHover&mkt=zh-CN"
	_, body, err := f.fetchHTML(ctx, u, "https://www.bing.com/")
	if err != nil {
		return nil
	}
	return parseBingImages(body)
}

func (f *ImageFetcher) searchBaiduImages(ctx context.Context, query string) []ImageResult {
	q := url.QueryEscape(query)
	u := "https://image.baidu.com/search/acjson?tn=resultjson_com&ipn=rj&ct=201326592&is=&fp=result&queryWord=" +
		q + "&word=" + q + "&pn=0&rn=30"

	f.baiduMu.Lock()
	defer f.baiduMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if !f.baiduCookies {
			// Baidu answers "Forbid spider access" without a homepage cookie.
			if _, _, err := f.fetchHTML(ctx, "https://image.baidu.com/", ""); err != nil {
				return nil
			}
			f.baiduCookies = true
		}
		body, err := f.fetchJSON(ctx, u, "https://image.baidu.com/")
		if err != nil {
			f.baiduCookies = false
			continue
		}
		var probe struct {
			AntiFlag int `json:"antiFlag"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.AntiFlag != 0 {
			f.baiduCookies = false
			continue
		}
		if rs := parseBaiduImages(body); len(rs) > 0 {
			return rs
		}
		break
	}
	return nil
}

// bingItemRE pairs a full-size URL with its title inside Bing's JSON metadata.
var bingItemRE = regexp.MustCompile(`"murl":"([^"]+)"[^{}]*?"t":"([^"]*)"`)

// parseBingImages extracts full-size media URLs from Bing's result HTML. The
// metadata lives in an `m="{&quot;murl&quot;:...}"` attribute, so the page must be
// HTML-unescaped before matching.
func parseBingImages(body []byte) []ImageResult {
	page := html.UnescapeString(string(body))
	var out []ImageResult
	seen := map[string]bool{}
	add := func(raw, title string) {
		raw = strings.ReplaceAll(raw, `\/`, `/`)
		if !strings.HasPrefix(raw, "http") || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, ImageResult{Title: title, URL: raw, Referer: "https://www.bing.com/"})
	}
	for _, m := range bingItemRE.FindAllStringSubmatch(page, -1) {
		add(m[1], m[2])
	}
	// Some entries have no title; keep their URLs too.
	for _, m := range mediaURLRE.FindAllStringSubmatch(page, -1) {
		add(m[1], "")
	}
	return out
}

// parseBaiduImages parses Baidu's acjson response.
func parseBaiduImages(body []byte) []ImageResult {
	var raw struct {
		Data []struct {
			ThumbURL         string `json:"thumbURL"`
			MiddleURL        string `json:"middleURL"`
			FromPageTitleEnc string `json:"fromPageTitleEnc"`
			ReplaceURL       []struct {
				ObjURL string `json:"ObjURL"`
			} `json:"replaceUrl"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	var out []ImageResult
	for _, d := range raw.Data {
		u := ""
		if len(d.ReplaceURL) > 0 {
			u = d.ReplaceURL[0].ObjURL
		}
		if u == "" {
			u = d.MiddleURL
		}
		if u == "" {
			u = d.ThumbURL
		}
		if u == "" || !strings.HasPrefix(u, "http") {
			continue
		}
		out = append(out, ImageResult{
			Title:   d.FromPageTitleEnc,
			URL:     u,
			Referer: "https://image.baidu.com/",
		})
	}
	return out
}
