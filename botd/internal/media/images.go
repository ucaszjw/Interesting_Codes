package media

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ImageFetcher downloads remote images with a size limit and SSRF protection.
type ImageFetcher struct {
	AllowPrivate bool
	MaxBytes     int64
	HTTP         *http.Client
}

// NewImageFetcher creates a fetcher. allowPrivate is only for tests: production
// must not let the model reach loopback/private addresses.
func NewImageFetcher(allowPrivate bool) *ImageFetcher {
	f := &ImageFetcher{AllowPrivate: allowPrivate, MaxBytes: 8 << 20}
	f.HTTP = &http.Client{
		Timeout: 20 * time.Second,
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
		return fmt.Errorf("仅支持 http(s) 图片地址")
	}
	if u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("非法的图片地址")
	}
	if f.AllowPrivate {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("无法解析图片域名：%w", err)
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() ||
			ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() || ip.IP.IsMulticast() {
			return fmt.Errorf("禁止访问内网地址")
		}
	}
	return nil
}

// Fetch downloads an image and returns its MIME type and bytes.
func (f *ImageFetcher) Fetch(ctx context.Context, rawURL string) (string, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("图片地址无效：%w", err)
	}
	if err := f.checkURL(ctx, u); err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := f.HTTP.Do(req)
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
