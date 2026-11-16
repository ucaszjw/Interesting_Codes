package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMoodMarker(t *testing.T) {
	for _, tc := range []struct {
		in    string
		clean string
		delta int
	}{
		{"在呢 [mood:+1]", "在呢", 1},
		{"[mood:-2] 烦死了", "烦死了", -2},
		{"好 [mood:+2] 呀 [mood:-1]", "好  呀", -1}, // last marker wins
		{"没标记", "没标记", 0},
		{"[mood:+9]", "", 2}, // clamped
	} {
		clean, delta := parseMoodMarker(tc.in)
		if clean != tc.clean || delta != tc.delta {
			t.Errorf("parseMoodMarker(%q) = %q,%d want %q,%d", tc.in, clean, delta, tc.clean, tc.delta)
		}
	}
}

func TestMoodTrackerApply(t *testing.T) {
	m := newMoodTracker("")
	if got := m.Current(1); got != "一般" {
		t.Fatalf("fresh mood = %q", got)
	}
	m.Apply(1, 2)
	if got := m.Current(1); got != "不错" {
		t.Errorf("after +2 = %q", got)
	}
	m.Apply(1, -2)
	m.Apply(1, -2)
	if got := m.Current(1); got != "烦躁" {
		t.Errorf("after -4 = %q", got)
	}
	m.Apply(1, 2)
	m.Apply(1, 2)
	m.Apply(1, 2)
	if got := m.Current(1); got != "不错" {
		t.Errorf("after recovery = %q", got)
	}
}

func TestMoodResetAfterIdle(t *testing.T) {
	m := newMoodTracker("")
	m.Apply(1, 2)
	m.mu.Lock()
	m.groups[1].updated = time.Now().Add(-31 * time.Minute)
	m.mu.Unlock()
	if got := m.Current(1); got != "一般" {
		t.Errorf("idle mood = %q, want 一般", got)
	}
}

func TestMoodPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mood.json")
	m := newMoodTracker(path)
	m.Apply(100, 2)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mood file: %v", err)
	}

	loaded := newMoodTracker(path)
	if got := loaded.Current(100); got != "不错" {
		t.Errorf("loaded mood = %q", got)
	}

	// An entry older than the idle window is discarded on load.
	loaded.mu.Lock()
	loaded.groups[100].updated = time.Now().Add(-31 * time.Minute)
	loaded.saveLocked()
	loaded.mu.Unlock()
	if got := newMoodTracker(path).Current(100); got != "一般" {
		t.Errorf("expired mood = %q", got)
	}
}
