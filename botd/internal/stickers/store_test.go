package stickers

import "testing"

func TestAddDedupesAndCaps(t *testing.T) {
	s, err := NewStore(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, added, err := s.Add("image/png", []byte("aaa"), "[动画表情]"); !added || err != nil {
		t.Fatalf("first add: %v %v", added, err)
	}
	if _, added, _ := s.Add("image/png", []byte("aaa"), "[动画表情]"); added {
		t.Error("duplicate was added")
	}
	_, _, _ = s.Add("image/png", []byte("bbb"), "")
	_, _, _ = s.Add("image/png", []byte("ccc"), "")
	if s.Count() != 2 {
		t.Fatalf("count = %d, want 2 (capped)", s.Count())
	}
}

func TestSetLabel(t *testing.T) {
	s, _ := NewStore(t.TempDir(), 10)
	id, _, err := s.Add("image/gif", []byte("gif1"), "[动画表情]")
	if err != nil {
		t.Fatal(err)
	}
	if !s.SetLabel(id, "狗头大笑") {
		t.Fatal("SetLabel returned false for existing id")
	}
	if s.SetLabel("nope", "x") {
		t.Error("SetLabel should reject unknown id")
	}
	e, _, ok := s.Find("狗头")
	if !ok || e.Label != "狗头大笑" {
		t.Fatalf("find = %+v %v", e, ok)
	}
}

func TestEntriesAndRead(t *testing.T) {
	s, _ := NewStore(t.TempDir(), 10)
	id, _, _ := s.Add("image/png", []byte("payload"), "[动画表情]")

	entries := s.Entries()
	if len(entries) != 1 || entries[0].ID != id || entries[0].Label != "[动画表情]" {
		t.Fatalf("entries = %+v", entries)
	}
	e, data, ok := s.Read(id)
	if !ok || e.ID != id || string(data) != "payload" {
		t.Fatalf("read = %+v %q %v", e, data, ok)
	}
	if _, _, ok := s.Read("missing"); ok {
		t.Error("Read should fail for unknown id")
	}
}

func TestFindByLabel(t *testing.T) {
	s, _ := NewStore(t.TempDir(), 10)
	_, _, _ = s.Add("image/png", []byte("png1"), "[/呲牙]")
	_, _, _ = s.Add("image/png", []byte("png2"), "[/大哭]")

	e, data, ok := s.Find("呲牙")
	if !ok || e.Label != "[/呲牙]" || string(data) != "png1" {
		t.Fatalf("find = %+v %q %v", e, data, ok)
	}
	if _, _, ok := s.Find("不存在"); !ok {
		t.Error("fallback to random should still find an entry")
	}
}
