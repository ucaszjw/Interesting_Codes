package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The dictator's mood is chosen by the model itself: every reply carries an
// [mood:N] marker (stripped before delivery) whose delta is applied here. If a
// group has no interaction for 30 minutes the mood resets to neutral.
const (
	moodHappyScore = 2
	moodAngryScore = -2
	moodMax        = 5
	moodIdleReset  = 30 * time.Minute
)

// moodMarkerRE matches the hidden per-reply mood delta.
var moodMarkerRE = regexp.MustCompile(`\[mood:([+-]?\d+)\]`)

// parseMoodMarker strips every mood marker and returns the last delta, clamped
// to ±2. A missing marker means no change.
func parseMoodMarker(reply string) (string, int) {
	delta := 0
	found := false
	for _, m := range moodMarkerRE.FindAllStringSubmatch(reply, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil {
			delta = v
			found = true
		}
	}
	clean := strings.TrimSpace(moodMarkerRE.ReplaceAllString(reply, ""))
	if !found {
		return clean, 0
	}
	if delta > 2 {
		delta = 2
	} else if delta < -2 {
		delta = -2
	}
	return clean, delta
}

type moodState struct {
	score   int
	updated time.Time
}

type moodTracker struct {
	mu     sync.Mutex
	groups map[int64]*moodState
	path   string // "" disables persistence
}

func newMoodTracker(path string) *moodTracker {
	m := &moodTracker{groups: map[int64]*moodState{}, path: path}
	m.load()
	return m
}

// Apply changes a group's mood and restarts the idle timer. It also counts as
// interaction when delta is 0.
func (m *moodTracker) Apply(groupID int64, delta int) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.groups[groupID]
	if st == nil {
		st = &moodState{}
		m.groups[groupID] = st
	}
	if st.updated.IsZero() || now.Sub(st.updated) >= moodIdleReset {
		st.score = 0
	}
	if delta > 2 {
		delta = 2
	} else if delta < -2 {
		delta = -2
	}
	st.score += delta
	if st.score > moodMax {
		st.score = moodMax
	} else if st.score < -moodMax {
		st.score = -moodMax
	}
	st.updated = now
	m.saveLocked()
	slog.Info("mood changed", "group", groupID, "delta", delta,
		"score", st.score, "mood", moodLabel(st.score))
}

func moodLabel(score int) string {
	switch {
	case score >= moodHappyScore:
		return "不错"
	case score <= moodAngryScore:
		return "烦躁"
	default:
		return "一般"
	}
}

// Current maps the decayed score to the mood label.
func (m *moodTracker) Current(groupID int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.groups[groupID]
	if st == nil || time.Since(st.updated) >= moodIdleReset {
		return "一般"
	}
	return moodLabel(st.score)
}

type moodFileEntry struct {
	Score     int   `json:"score"`
	UpdatedAt int64 `json:"updated_at"`
}

func (m *moodTracker) load() {
	if m.path == "" {
		return
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var raw map[string]moodFileEntry
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	now := time.Now()
	for key, e := range raw {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			continue
		}
		updated := time.Unix(e.UpdatedAt, 0)
		if now.Sub(updated) >= moodIdleReset {
			continue // expired while the bot was down
		}
		m.groups[id] = &moodState{score: e.Score, updated: updated}
	}
}

func (m *moodTracker) saveLocked() {
	if m.path == "" {
		return
	}
	raw := make(map[string]moodFileEntry, len(m.groups))
	for id, st := range m.groups {
		raw[strconv.FormatInt(id, 10)] = moodFileEntry{Score: st.score, UpdatedAt: st.updated.Unix()}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}
