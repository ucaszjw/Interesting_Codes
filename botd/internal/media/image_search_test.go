package media

import "testing"

func TestParseBingImages(t *testing.T) {
	// Real Bing pages put the JSON in an HTML-escaped attribute:
	// m="{&quot;murl&quot;:&quot;https:\/\/...&quot;}".
	body := []byte(`<a class="iusc" m="{&quot;murl&quot;:&quot;https:\/\/cdn.example\/full.jpg&quot;,&quot;t&quot;:&quot;x&quot;}"></a>
<a class="iusc" m="{&quot;murl&quot;:&quot;https:\/\/cdn.example\/b.png&quot;}"></a>`)
	rs := parseBingImages(body)
	if len(rs) != 2 || rs[0].URL != "https://cdn.example/full.jpg" {
		t.Fatalf("results = %+v", rs)
	}
	if rs[0].Referer != "https://www.bing.com/" {
		t.Errorf("referer = %q", rs[0].Referer)
	}
}

func TestParseBaiduImages(t *testing.T) {
	body := []byte(`{"data":[
      {"replaceUrl":[{"ObjURL":"https://img.example/original.jpg"}],"middleURL":"https://img.example/mid.jpg","fromPageTitleEnc":"钟楼"},
      {"replaceUrl":[{"ObjURL":""}],"middleURL":"https://img.example/mid2.jpg"},
      {"middleURL":"","thumbURL":"https://img.example/thumb3.jpg"},
      {"middleURL":"","thumbURL":""}
    ]}`)
	rs := parseBaiduImages(body)
	if len(rs) != 3 {
		t.Fatalf("results = %+v", rs)
	}
	if rs[0].URL != "https://img.example/original.jpg" || rs[0].Title != "钟楼" {
		t.Errorf("first = %+v", rs[0])
	}
	if rs[1].URL != "https://img.example/mid2.jpg" {
		t.Errorf("second = %+v", rs[1])
	}
	if rs[2].URL != "https://img.example/thumb3.jpg" {
		t.Errorf("third = %+v", rs[2])
	}
	if rs[0].Referer != "https://image.baidu.com/" {
		t.Errorf("referer = %q", rs[0].Referer)
	}
}
