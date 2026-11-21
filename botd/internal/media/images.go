package media

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0 Safari/537.36"

// ImageFetcher downloads remote images and extracts links from HTML pages, with
// a size limit and SSRF protection.
type ImageFetcher struct {
	AllowPrivate bool
	MaxBytes     int64
	HTTP         *http.Client

	// Baidu blocks spider-like requests. Visiting the homepage first stores its
	// cookies in the jar, which makes its acjson endpoint work.
	baiduMu      sync.Mutex
	baiduCookies bool
}

// NewImageFetcher creates a fetcher. allowPrivate is only for tests: production
// must not let the model reach loopback/private addresses.
func NewImageFetcher(allowPrivate bool) *ImageFetcher {
	f := &ImageFetcher{AllowPrivate: allowPrivate, MaxBytes: 8 << 20}
	jar, _ := cookiejar.New(nil)
	f.HTTP = &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return f.checkURL(req.Context(), req.URL)
		},
	}
	return f
}

func (f *ImageFetcher) checkURL(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http(s) 地址")
	}
	if u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("非法的地址")
	}
	if f.AllowPrivate {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("无法解析域名：%w", err)
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() ||
			ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() || ip.IP.IsMulticast() {
			return fmt.Errorf("禁止访问内网地址")
		}
	}
	return nil
}

func (f *ImageFetcher) get(ctx context.Context, rawURL, referer string) (*http.Response, error) {
	return f.getWithAccept(ctx, rawURL, referer, "image/avif,image/webp,image/*,*/*;q=0.8")
}

func (f *ImageFetcher) getWithAccept(ctx context.Context, rawURL, referer, accept string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("地址无效：%w", err)
	}
	if err := f.checkURL(ctx, u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", accept)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return f.HTTP.Do(req)
}

// fetchJSON requests a JSON endpoint (Baidu's acjson).
func (f *ImageFetcher) fetchJSON(ctx context.Context, rawURL, referer string) ([]byte, error) {
	resp, err := f.getWithAccept(ctx, rawURL, referer, "application/json, text/plain, */*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// Fetch downloads an image and returns its MIME type and bytes. referer is
// optional and helps with hotlink-protected CDNs (e.g. pixiv).
func (f *ImageFetcher) Fetch(ctx context.Context, rawURL, referer string) (string, []byte, error) {
	resp, err := f.get(ctx, rawURL, referer)
	if err != nil {
		return "", nil, fmt.Errorf("下载图片失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("下载图片失败：HTTP %d", resp.StatusCode)
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", nil, fmt.Errorf("读取图片失败：%w", err)
	}
	if int64(len(data)) > limit {
		return "", nil, fmt.Errorf("图片超过 %d MB", limit>>20)
	}
	mime := http.DetectContentType(data)
	if len(mime) < 6 || mime[:6] != "image/" {
		return "", nil, fmt.Errorf("地址返回的不是图片（%s）", mime)
	}
	return mime, data, nil
}

// FetchHTML downloads a page and returns its final URL and body.
func (f *ImageFetcher) FetchHTML(ctx context.Context, rawURL string) (string, []byte, error) {
	return f.fetchHTML(ctx, rawURL, "")
}

func (f *ImageFetcher) fetchHTML(ctx context.Context, rawURL, referer string) (string, []byte, error) {
	resp, err := f.get(ctx, rawURL, referer)
	if err != nil {
		return "", nil, fmt.Errorf("抓取网页失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("抓取网页失败：HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", nil, fmt.Errorf("读取网页失败：%w", err)
	}
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return final, body, nil
}

var (
	imgTagRE  = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	attrRE    = regexp.MustCompile(`(?is)\b(?:src|data-src|data-original)\s*=\s*["']([^"']+)["']`)
	srcsetRE  = regexp.MustCompile(`(?is)\bsrcset\s*=\s*["']([^"']+)["']`)
	ogImageRE = regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)\s*=\s*["']og:image["'][^>]*content\s*=\s*["']([^"']+)["']`)
	// Bing image search embeds full-size media URLs as "murl":"https:\/\/...".
	mediaURLRE = regexp.MustCompile(`"murl"\s*:\s*"([^"]+)"`)
)

// ExtractImageURLs finds image links in HTML and resolves them against base.
// Results are deduplicated and capped at max.
func ExtractImageURLs(baseURL string, body []byte, max int) []string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	if max <= 0 {
		max = 10
	}
	// Bing and friends embed JSON attributes with HTML entities.
	page := html.UnescapeString(string(body))
	seen := map[string]bool{}
	var out []string

	add := func(raw string) {
		raw = strings.TrimSpace(html.UnescapeString(raw))
		if raw == "" || strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "javascript:") {
			return
		}
		// srcset: "url 1x, url2 2x" — take the first candidate.
		if i := strings.Index(raw, " "); i > 0 && !strings.Contains(raw[:i], ",") {
			raw = raw[:i]
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		if !u.IsAbs() {
			u = base.ResolveReference(u)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		s := u.String()
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	if m := ogImageRE.FindStringSubmatch(page); len(m) == 2 {
		add(m[1])
	}
	// Image-search result pages (Bing) carry full-size URLs in JSON.
	for _, m := range mediaURLRE.FindAllStringSubmatch(page, -1) {
		raw := strings.ReplaceAll(m[1], `\/`, `/`)
		add(raw)
		if len(out) >= max {
			return out
		}
	}
	for _, tag := range imgTagRE.FindAllString(page, -1) {
		if m := attrRE.FindStringSubmatch(tag); len(m) == 2 {
			add(m[1])
		}
		if m := srcsetRE.FindStringSubmatch(tag); len(m) == 2 {
			add(m[1])
		}
		if len(out) >= max {
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}
