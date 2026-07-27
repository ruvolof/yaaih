// Package skills loads markdown "skill" files from a directory next to the
// agent.md and renders them into a single block appended to the system prompt.
//
// A skill is *policy*, not *capability*. It teaches the model conventions,
// guardrails, or workflows; it does not add anything to the tool list. Skills
// are loaded once at startup — there is no hot reload in this batch.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/frontmatter"

	"github.com/ruvolof/aibitat-harness-go/internal/logging"
)

var log = logging.For("skills")

// Skill is one loaded skill file.
type Skill struct {
	Name        string
	Description string
	Path        string // absolute path to the source file
	Body        string // markdown body, trimmed
	Disabled    bool
}

// skillFrontmatter is the YAML header of a skill file.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Disabled    bool   `yaml:"disabled"`
}

// Load walks dir (non-recursive) for *.md files, parses each, and returns the
// enabled ones sorted by filename. A missing dir returns (nil, nil) — absence
// of the directory is not an error. Per-file failures (unreadable, malformed
// frontmatter) are logged and skipped; they don't fail the whole load.
func Load(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir %s: %w", dir, err)
	}

	// Sort entries by filename so output order is deterministic.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var out []Skill
	for _, name := range names {
		abs := filepath.Join(dir, name)
		sk, ok := loadOne(abs)
		if !ok {
			continue
		}
		if sk.Disabled {
			log.Info("skill disabled, skipping", "path", abs, "name", sk.Name)
			continue
		}
		out = append(out, sk)
	}
	return out, nil
}

// loadOne parses a single skill file. Returns ok=false (after logging) on any
// per-file error so the caller can keep going with the rest of the batch.
func loadOne(abs string) (Skill, bool) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		log.Warn("skill read failed, skipping", "path", abs, "err", err.Error())
		return Skill{}, false
	}

	md := goldmark.New(goldmark.WithExtensions(&frontmatter.Extender{}))
	ctx := parser.NewContext()
	md.Parser().Parse(text.NewReader(raw), parser.WithContext(ctx))

	fm := frontmatter.Get(ctx)
	if fm == nil {
		log.Warn("skill missing frontmatter, skipping", "path", abs)
		return Skill{}, false
	}
	var meta skillFrontmatter
	if err := fm.Decode(&meta); err != nil {
		log.Warn("skill frontmatter decode failed, skipping", "path", abs, "err", err.Error())
		return Skill{}, false
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		// Fall back to the filename (without extension) so an unnamed skill
		// still renders with a stable identifier rather than name="".
		name = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	}

	return Skill{
		Name:        name,
		Description: strings.TrimSpace(meta.Description),
		Path:        abs,
		Body:        strings.TrimSpace(extractBody(raw)),
		Disabled:    meta.Disabled,
	}, true
}

// extractBody returns the markdown content after the closing "---" of the YAML
// frontmatter. Mirrors config.extractBody so skill bodies are extracted the
// same way as the agent.md body. If no frontmatter delimiters are found, the
// whole input is returned.
func extractBody(raw []byte) string {
	s := string(raw)
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[len("---"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return s
	}
	body := rest[idx+len("\n---"):]
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	}
	return body
}

// Format renders skills as a single block to append to a system prompt.
// Returns "" if skills is empty. The shape is deterministic and easy for the
// model to parse:
//
//	<skills>
//	<skill name="general-style">
//	...body...
//	</skill>
//	</skills>
func Format(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<skills>\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "<skill name=%q>\n", s.Name)
		b.WriteString(s.Body)
		b.WriteString("\n</skill>\n")
	}
	b.WriteString("</skills>")
	return b.String()
}
