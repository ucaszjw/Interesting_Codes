package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractImageURLs(t *testing.T) {
	page := `<html><head>
<meta property="og:image" content="/og.png">
</head><body>
<img src="a.jpg" alt="a">
<img data-src="//cdn.example/b.jpg">
<img srcset="c.webp 1x, d.webp 2x">
<img src="data:image/png;base64,xxx">
</body></html>`

	urls := ExtractImageURLs("https://example.com/page", []byte(page), 10)
	want := []string{
		"https://example.com/og.png",
		"https://example.com/a.jpg",
		"https://cdn.example/b.jpg",
		"https://example.com/c.webp",
	}
	if len(urls) != len(want) {
		t.Fatalf("urls = %v", urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Errorf("urls[%d] = %q, want %q", i, urls[i], want[i])
		}
	}
}

func TestFetchSendsBrowserHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla/") {
			t.Errorf("user agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Referer") != "https://www.pixiv.net/" {
			t.Errorf("referer = %q", r.Header.Get("Referer"))
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	}))
	defer srv.Close()

	f := NewImageFetcher(true)
	mime, data, err := f.Fetch(context.Background(), srv.URL, "https://www.pixiv.net/")
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || len(data) == 0 {
		t.Errorf("mime=%q len=%d", mime, len(data))
	}
}

func TestFetchRejectsNonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()
	if _, _, err := NewImageFetcher(true).Fetch(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<img src="/x.png">`))
	}))
	defer srv.Close()
	finalURL, body, err := NewImageFetcher(true).FetchHTML(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if finalURL != srv.URL || !strings.Contains(string(body), "x.png") {
		t.Errorf("final=%q body=%q", finalURL, body)
	}
}

func TestExtractImageURLs_BingMediaURL(t *testing.T) {
	page := `{"murl":"https:\/\/example.com\/full.jpg","turl":"https:\/\/example.com\/thumb.jpg"}`
	urls := ExtractImageURLs("https://www.bing.com/images/search?q=x", []byte(page), 10)
	if len(urls) != 1 || urls[0] != "https://example.com/full.jpg" {
		t.Fatalf("urls = %v", urls)
	}
}
