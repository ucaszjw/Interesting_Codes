// Package stickers keeps a local library of memes seen in group chats.
package stickers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one sticker file.
type Entry struct {
	ID      string    `json:"id"`
	File    string    `json:"file"`
	Mime    string    `json:"mime"`
	Size    int64     `json:"size"`
	Label   string    `json:"label,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// Store is a capped on-disk sticker library with deduplication.
type Store struct {
	Dir     string
	Max     int
	mu      sync.Mutex
	entries []Entry
}

// NewStore loads (or creates) the library in dir.
func NewStore(dir string, max int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("stickers: %w", err)
	}
	if max <= 0 {
		max = 300
	}
	s := &Store{Dir: dir, Max: max}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err == nil {
		_ = json.Unmarshal(data, &s.entries)
	}
	return s, nil
}

func (s *Store) indexFile() string { return filepath.Join(s.Dir, "index.json") }

func (s *Store) saveIndex() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexFile())
}

func extFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

// Add stores one sticker. It reports false when the same image is already known.
func (s *Store) Add(mime string, data []byte, label string) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:12])

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID == id {
			return false, nil
		}
	}
	file := id + extFor(mime)
	if err := os.WriteFile(filepath.Join(s.Dir, file), data, 0o644); err != nil {
		return false, fmt.Errorf("stickers: write: %w", err)
	}
	s.entries = append(s.entries, Entry{
		ID: id, File: file, Mime: mime, Size: int64(len(data)),
		Label: label, AddedAt: time.Now(),
	})
	for len(s.entries) > s.Max {
		old := s.entries[0]
		s.entries = s.entries[1:]
		_ = os.Remove(filepath.Join(s.Dir, old.File))
	}
	if err := s.saveIndex(); err != nil {
		return false, err
	}
	return true, nil
}

// Count returns the library size.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Find returns a sticker whose label contains query; falls back to random.
func (s *Store) Find(query string) (Entry, []byte, bool) {
	s.mu.Lock()
	entries := append([]Entry(nil), s.entries...)
	s.mu.Unlock()
	if len(entries) == 0 {
		return Entry{}, nil, false
	}
	query = strings.TrimSpace(query)
	var pool []Entry
	if query != "" {
		for _, e := range entries {
			if strings.Contains(e.Label, query) {
				pool = append(pool, e)
			}
		}
	}
	if len(pool) == 0 {
		pool = entries
	}
	e := pool[rand.Intn(len(pool))]
	data, err := os.ReadFile(filepath.Join(s.Dir, e.File))
	if err != nil {
		return Entry{}, nil, false
	}
	return e, data, true
}
