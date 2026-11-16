package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSynthesize(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/tts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer k" {
			t.Errorf("auth = %q", auth)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte("RIFFwav"))
	}))
	defer srv.Close()

	tts := NewTTS("k", srv.URL+"/v1", "tide")
	audio, format, err := tts.Synthesize(context.Background(), "你好")
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "RIFFwav" || format != "wav" {
		t.Errorf("audio=%q format=%q", audio, format)
	}
	if got["text"] != "你好" || got["voice_id"] != "tide" || got["filter_markdown"] != true {
		t.Errorf("body = %v", got)
	}
}

func TestSynthesizeEmptyText(t *testing.T) {
	if _, _, err := NewTTS("k", "", "").Synthesize(context.Background(), " "); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSynthesizeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("bad key"))
	}))
	defer srv.Close()
	_, _, err := NewTTS("k", srv.URL, "").Synthesize(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}
