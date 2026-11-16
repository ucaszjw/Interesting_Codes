package stickers

import "testing"

func TestAddDedupesAndCaps(t *testing.T) {
	s, err := NewStore(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.Add("image/png", []byte("aaa"), "[动画表情]")
	if err != nil || !added {
		t.Fatalf("first add: %v %v", added, err)
	}
	if added, _ := s.Add("image/png", []byte("aaa"), "[动画表情]"); added {
		t.Error("duplicate was added")
	}
	s.Add("image/png", []byte("bbb"), "")
	s.Add("image/png", []byte("ccc"), "")
	if s.Count() != 2 {
		t.Fatalf("count = %d, want 2 (capped)", s.Count())
	}
}

func TestFindByLabel(t *testing.T) {
	s, _ := NewStore(t.TempDir(), 10)
	s.Add("image/png", []byte("png1"), "[/呲牙]")
	s.Add("image/png", []byte("png2"), "[/大哭]")

	e, data, ok := s.Find("呲牙")
	if !ok || e.Label != "[/呲牙]" || string(data) != "png1" {
		t.Fatalf("find = %+v %q %v", e, data, ok)
	}
	if _, _, ok := s.Find("不存在"); !ok {
		t.Error("fallback to random should still find an entry")
	}
}
