// Package skills loads markdown "skill" files from a directory next to the
// agent.md and composes them into the system prompt.
//
// A skill is *policy*, not *capability*. It teaches the model conventions,
// guardrails, or workflows; it does not add anything to the tool list.
//
// Most skills are loaded on demand: the system prompt carries only their name
// and description, and a skill's body joins it once the model or the operator
// asks for it. A skill marked always, or one without a description for the
// model to choose it by, is part of the system prompt from the start.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ruvolof/yaaih/internal/frontmatter"
	"github.com/ruvolof/yaaih/internal/logging"
)

var log = logging.For("skills")

// Skill is one loaded skill file.
type Skill struct {
	Name        string
	Description string
	Path        string // absolute path to the source file
	Body        string // markdown body, trimmed
	Always      bool
	Disabled    bool
}

// OnDemand reports whether the skill waits to be loaded rather than being part
// of the system prompt from the start. A skill without a description cannot be
// on demand: the description is all the model has to decide by.
func (s Skill) OnDemand() bool { return !s.Always && s.Description != "" }

// skillFrontmatter is the YAML header of a skill file.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Always      bool   `yaml:"always"`
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
	seen := map[string]string{}
	for _, name := range names {
		abs := filepath.Join(dir, name)
		skill, ok := loadOne(abs)
		if !ok {
			continue
		}
		if skill.Disabled {
			log.Info("skill disabled, skipping", "path", abs, "name", skill.Name)
			continue
		}
		// The name is what the model and the operator load a skill by, so a
		// second skill answering to it could never be reached.
		if first, taken := seen[skill.Name]; taken {
			log.Warn("skill name already used, skipping", "path", abs, "name", skill.Name, "first", first)
			continue
		}
		seen[skill.Name] = abs
		if !skill.Always && skill.Description == "" {
			log.Info("skill has no description, loading it from the start", "path", abs, "name", skill.Name)
		}
		out = append(out, skill)
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

	var meta skillFrontmatter
	body, err := frontmatter.Parse(raw, &meta)
	if errors.Is(err, frontmatter.ErrMissing) {
		log.Warn("skill missing frontmatter, skipping", "path", abs)
		return Skill{}, false
	}
	if err != nil {
		log.Warn("skill frontmatter invalid, skipping", "path", abs, "err", err.Error())
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
		Body:        strings.TrimSpace(body),
		Always:      meta.Always,
		Disabled:    meta.Disabled,
	}, true
}

// Find returns the skill called name.
func Find(all []Skill, name string) (Skill, bool) {
	for _, skill := range all {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// catalogIntro tells the model what the catalog is and what to do with it.
const catalogIntro = "Skills are instructions for particular kinds of task. Before you start a task that " +
	"matches one of the descriptions below, call skill_load with its name: its instructions then join " +
	"this system prompt for the rest of the session."

// Compose builds the system prompt from the agent's own prompt and its skills:
// a catalog of the on-demand skills not loaded yet, then the bodies of the
// skills that are in effect — those loaded from the start, and those named in
// loaded. Either block is left out when it would be empty, so an agent without
// skills gets base back unchanged.
//
// Bodies follow the order of all rather than the order skills were loaded in,
// so the same set of skills always produces the same prompt:
//
//	<available_skills>
//	...intro...
//	<skill name="network-debug">description</skill>
//	</available_skills>
//
//	<skills>
//	<skill name="general-style">
//	...body...
//	</skill>
//	</skills>
func Compose(base string, all []Skill, loaded []string) string {
	isLoaded := make(map[string]bool, len(loaded))
	for _, name := range loaded {
		isLoaded[name] = true
	}

	var catalog, bodies strings.Builder
	for _, skill := range all {
		if skill.OnDemand() && !isLoaded[skill.Name] {
			fmt.Fprintf(&catalog, "<skill name=%q>%s</skill>\n", skill.Name, skill.Description)
			continue
		}
		fmt.Fprintf(&bodies, "<skill name=%q>\n%s\n</skill>\n", skill.Name, skill.Body)
	}

	prompt := base
	if catalog.Len() > 0 {
		prompt += "\n\n<available_skills>\n" + catalogIntro + "\n" + catalog.String() + "</available_skills>"
	}
	if bodies.Len() > 0 {
		prompt += "\n\n<skills>\n" + bodies.String() + "</skills>"
	}
	return prompt
}
