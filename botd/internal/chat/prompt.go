package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Prompts loads the layered system prompt: shared core, optional per-group
// context, and one persona. Order matters for provider prefix caching, so the
// most-shared content always comes first.
type Prompts struct {
	Dir      string
	Core     string
	Groups   map[string]string
	Personas map[string]string
}

// LoadPrompts reads prompts/core.md, prompts/groups/<id>.md and
// prompts/personas/<name>.md (all optional except core).
func LoadPrompts(dir string) (*Prompts, error) {
	p := &Prompts{Dir: dir, Groups: map[string]string{}, Personas: map[string]string{}}

	core, err := os.ReadFile(filepath.Join(dir, "core.md"))
	if err != nil {
		return nil, fmt.Errorf("prompts: read core.md: %w", err)
	}
	p.Core = strings.TrimSpace(string(core))

	for _, kind := range []string{"groups", "personas"} {
		entries, err := os.ReadDir(filepath.Join(dir, kind))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, kind, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("prompts: read %s/%s: %w", kind, e.Name(), err)
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if kind == "groups" {
				p.Groups[name] = strings.TrimSpace(string(body))
			} else {
				p.Personas[name] = strings.TrimSpace(string(body))
			}
		}
	}
	return p, nil
}

// PersonaNames lists personas in a stable order.
func (p *Prompts) PersonaNames() []string {
	names := make([]string, 0, len(p.Personas))
	for n := range p.Personas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// System assembles the system prompt for one session. The core block stays
// byte-identical across sessions so it can stay in the provider's prefix cache;
// only the tail varies with group and persona.
func (p *Prompts) System(groupKey, persona string) string {
	var b strings.Builder
	b.WriteString(p.Core)
	if g := p.Groups[groupKey]; g != "" {
		b.WriteString("\n\n## 本群补充设定\n\n")
		b.WriteString(g)
	}
	if persona != "" {
		body := p.Personas[persona]
		if body == "" {
			body = fmt.Sprintf("`[%s]`\n（该人设未定义，保持自然、简洁的聊天语气。）", persona)
		}
		b.WriteString("\n\n## 当前人设\n\n")
		b.WriteString(body)
	}
	return b.String()
}
