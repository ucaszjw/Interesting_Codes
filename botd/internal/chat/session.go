package chat

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Msg is one stored conversation turn.
type Msg struct {
	Role    string    `json:"role"` // "user" | "assistant" | "system"
	Name    string    `json:"name,omitempty"`
	UserID  string    `json:"user_id,omitempty"`
	Content string    `json:"content"`
	Images  int       `json:"images,omitempty"` // count only; bytes are not persisted
	At      time.Time `json:"at"`
}

// Session is one conversation thread inside a chat.
type Session struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	Key              string    `json:"key"` // chat key, e.g. qq:g:100
	Persona          string    `json:"persona"`
	Summary          string    `json:"summary,omitempty"`
	Facts            []string  `json:"facts,omitempty"`
	Messages         []Msg     `json:"messages"`
	LastPromptTokens int       `json:"last_prompt_tokens,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SessionInfo is a lightweight entry for /list.
type SessionInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
}

type chatIndex struct {
	Active   string        `json:"active"`
	Sessions []SessionInfo `json:"sessions"`
}

// Store persists sessions (and chat-level facts) as JSON.
type Store struct {
	Dir string
	mu  sync.Mutex
}

// NewStore creates the directory if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	return &Store{Dir: dir}, nil
}

func hashKey(key string) string {
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Store) chatDir(key string) string   { return filepath.Join(s.Dir, hashKey(key)) }
func (s *Store) indexPath(key string) string { return filepath.Join(s.chatDir(key), "index.json") }
func (s *Store) sessionPath(key, id string) string {
	return filepath.Join(s.chatDir(key), id+".json")
}

// loadIndex reads the chat index, migrating a pre-multi-session file if present.
func (s *Store) loadIndex(key string) chatIndex {
	var idx chatIndex
	data, err := os.ReadFile(s.indexPath(key))
	if err == nil {
		_ = json.Unmarshal(data, &idx)
		if idx.Active != "" {
			return idx
		}
	}

	// Migrate the old flat layout (<sha1>.json) into chatDir/s1.json.
	old := filepath.Join(s.Dir, hashKey(key)+".json")
	if oldData, err := os.ReadFile(old); err == nil {
		_ = os.MkdirAll(s.chatDir(key), 0o755)
		var sess Session
		if json.Unmarshal(oldData, &sess) == nil {
			sess.ID = "s1"
			sess.Key = key
			sess.UpdatedAt = time.Now()
			_ = s.writeSession(&sess)
			idx = chatIndex{Active: "s1", Sessions: []SessionInfo{{
				ID: "s1", UpdatedAt: sess.UpdatedAt, Messages: len(sess.Messages),
			}}}
			_ = s.writeIndex(key, idx)
			_ = os.Remove(old)
			return idx
		}
	}

	if idx.Active == "" {
		_ = os.MkdirAll(s.chatDir(key), 0o755)
		idx = chatIndex{Active: "s1", Sessions: []SessionInfo{{ID: "s1"}}}
		_ = s.writeIndex(key, idx)
	}
	return idx
}

func (s *Store) writeIndex(key string, idx chatIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath(key))
}

func (s *Store) writeSession(sess *Session) error {
	sess.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("session store: encode: %w", err)
	}
	p := s.sessionPath(sess.Key, sess.ID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("session store: write: %w", err)
	}
	return os.Rename(tmp, p)
}

// Load returns the chat's active session, creating one if needed.
func (s *Store) Load(key string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.loadIndex(key)
	data, err := os.ReadFile(s.sessionPath(key, idx.Active))
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{ID: idx.Active, Key: key}, nil
		}
		return nil, fmt.Errorf("session store: load %s/%s: %w", key, idx.Active, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session store: decode %s/%s: %w", key, idx.Active, err)
	}
	sess.Key = key
	if sess.ID == "" {
		sess.ID = idx.Active
	}
	for _, info := range idx.Sessions {
		if info.ID == sess.ID {
			sess.Name = info.Name
			break
		}
	}
	return &sess, nil
}

// Save writes the session and refreshes its index entry.
func (s *Store) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == "" {
		sess.ID = "s1"
	}
	if err := s.writeSession(sess); err != nil {
		return err
	}
	idx := s.loadIndex(sess.Key)
	idx.Active = sess.ID
	found := false
	for i := range idx.Sessions {
		if idx.Sessions[i].ID == sess.ID {
			idx.Sessions[i].Name = sess.Name
			idx.Sessions[i].UpdatedAt = sess.UpdatedAt
			idx.Sessions[i].Messages = len(sess.Messages)
			found = true
			break
		}
	}
	if !found {
		idx.Sessions = append(idx.Sessions, SessionInfo{
			ID: sess.ID, Name: sess.Name, UpdatedAt: sess.UpdatedAt, Messages: len(sess.Messages),
		})
	}
	return s.writeIndex(sess.Key, idx)
}

// List returns the chat's sessions in creation order.
func (s *Store) List(key string) []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadIndex(key).Sessions
}

// NewSession creates and activates a fresh thread in the chat.
func (s *Store) NewSession(key, name string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.loadIndex(key)
	max := 0
	for _, info := range idx.Sessions {
		if strings.HasPrefix(info.ID, "s") {
			if n, err := strconv.Atoi(strings.TrimPrefix(info.ID, "s")); err == nil && n > max {
				max = n
			}
		}
	}
	sess := &Session{ID: "s" + strconv.Itoa(max+1), Name: name, Key: key}
	idx.Active = sess.ID
	idx.Sessions = append(idx.Sessions, SessionInfo{ID: sess.ID, Name: name, UpdatedAt: time.Now()})
	if err := s.writeSession(sess); err != nil {
		return nil, err
	}
	if err := s.writeIndex(key, idx); err != nil {
		return nil, err
	}
	return sess, nil
}

// Switch activates a session by id, 1-based list position, or name.
func (s *Store) Switch(key, query string) (*Session, error) {
	s.mu.Lock()
	idx := s.loadIndex(key)
	target := ""
	if n, err := strconv.Atoi(query); err == nil && n >= 1 && n <= len(idx.Sessions) {
		target = idx.Sessions[n-1].ID
	}
	if target == "" {
		for _, info := range idx.Sessions {
			if info.ID == query {
				target = info.ID
				break
			}
		}
	}
	if target == "" {
		ql := strings.ToLower(query)
		for _, info := range idx.Sessions {
			if info.Name != "" && strings.Contains(strings.ToLower(info.Name), ql) {
				target = info.ID
				break
			}
		}
	}
	if target == "" {
		s.mu.Unlock()
		return nil, fmt.Errorf("没有匹配的会话：%s", query)
	}
	idx.Active = target
	if err := s.writeIndex(key, idx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	return s.Load(key)
}

// Rename sets the active session's display name.
func (s *Store) Rename(sess *Session, name string) error {
	sess.Name = name
	return s.Save(sess)
}

// Reset clears the conversation but keeps the persona, name and facts.
func (s *Store) Reset(sess *Session) {
	sess.Summary = ""
	sess.Messages = nil
	sess.LastPromptTokens = 0
}

// LoadFacts returns chat-level long-term facts.
func (s *Store) LoadFacts(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.chatDir(key), "facts.json"))
	if err != nil {
		return nil
	}
	var facts []string
	_ = json.Unmarshal(data, &facts)
	return facts
}

// SaveFacts writes chat-level long-term facts.
func (s *Store) SaveFacts(key string, facts []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.chatDir(key), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(facts, "", "  ")
	tmp := filepath.Join(s.chatDir(key), "facts.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.chatDir(key), "facts.json"))
}

// SortedIDs is a helper for deterministic listings.
func SortedIDs(infos []SessionInfo) []string {
	ids := make([]string, 0, len(infos))
	for _, i := range infos {
		ids = append(ids, i.ID)
	}
	sort.Strings(ids)
	return ids
}
