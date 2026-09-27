package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadValidSkill(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "style.md", "---\nname: general-style\ndescription: how to write code\n---\nAlways prefer ripgrep over grep.\n")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ss) != 1 {
		t.Fatalf("got %d skills, want 1", len(ss))
	}
	s := ss[0]
	if s.Name != "general-style" {
		t.Errorf("Name = %q, want general-style", s.Name)
	}
	if s.Description != "how to write code" {
		t.Errorf("Description = %q", s.Description)
	}
	if s.Body != "Always prefer ripgrep over grep." {
		t.Errorf("Body = %q", s.Body)
	}
	if !filepath.IsAbs(s.Path) {
		t.Errorf("Path = %q, want absolute", s.Path)
	}
}

func TestLoadSkipsDisabled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "on.md", "---\nname: on\n---\nenabled body\n")
	writeFile(t, dir, "off.md", "---\nname: off\ndisabled: true\n---\ndisabled body\n")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ss) != 1 || ss[0].Name != "on" {
		t.Fatalf("got %+v, want only the enabled skill", ss)
	}
}

func TestLoadMissingDir(t *testing.T) {
	ss, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Load missing dir returned err: %v", err)
	}
	if ss != nil {
		t.Fatalf("Load missing dir returned %v, want nil", ss)
	}
}

func TestLoadSortedByFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.md", "---\nname: bravo\n---\nB\n")
	writeFile(t, dir, "a.md", "---\nname: alpha\n---\nA\n")
	writeFile(t, dir, "c.md", "---\nname: charlie\n---\nC\n")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := []string{ss[0].Name, ss[1].Name, ss[2].Name}
	want := []string{"alpha", "bravo", "charlie"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestLoadMalformedSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.md", "---\nname: good\n---\nbody\n")
	// No frontmatter at all -> skipped, not fatal.
	writeFile(t, dir, "bad.md", "just some text, no frontmatter\n")
	// Non-markdown file is ignored entirely.
	writeFile(t, dir, "notes.txt", "ignored")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ss) != 1 || ss[0].Name != "good" {
		t.Fatalf("got %+v, want only the good skill", ss)
	}
}

func TestLoadAlwaysAndOnDemand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: pinned\ndescription: always there\nalways: true\n---\nP\n")
	writeFile(t, dir, "b.md", "---\nname: lazy\ndescription: loaded when asked\n---\nL\n")
	writeFile(t, dir, "c.md", "---\nname: undescribed\n---\nU\n")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]bool{"pinned": false, "lazy": true, "undescribed": false}
	for _, skill := range ss {
		if skill.OnDemand() != want[skill.Name] {
			t.Errorf("%s: OnDemand = %v, want %v", skill.Name, skill.OnDemand(), want[skill.Name])
		}
	}
	if !ss[0].Always {
		t.Errorf("pinned: Always = false, want true")
	}
}

func TestLoadSkipsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: same\n---\nfirst\n")
	writeFile(t, dir, "b.md", "---\nname: same\n---\nsecond\n")

	ss, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ss) != 1 || ss[0].Body != "first" {
		t.Fatalf("got %+v, want only the first skill named same", ss)
	}
}

func TestFind(t *testing.T) {
	all := []Skill{{Name: "alpha"}, {Name: "bravo"}}
	if skill, ok := Find(all, "bravo"); !ok || skill.Name != "bravo" {
		t.Errorf("Find(bravo) = %+v, %v", skill, ok)
	}
	if _, ok := Find(all, "charlie"); ok {
		t.Errorf("Find(charlie) found a skill that does not exist")
	}
}

func TestComposeWithoutSkillsReturnsBase(t *testing.T) {
	if got := Compose("base prompt", nil, nil); got != "base prompt" {
		t.Errorf("Compose = %q, want the base unchanged", got)
	}
}

func TestComposeEagerSkillsOnly(t *testing.T) {
	out := Compose("base", []Skill{
		{Name: "general-style", Body: "line one\nline two", Always: true},
		{Name: "network-debug", Body: "use curl -m 5"},
	}, nil)
	want := "base\n\n<skills>\n" +
		"<skill name=\"general-style\">\n" +
		"line one\nline two\n" +
		"</skill>\n" +
		"<skill name=\"network-debug\">\n" +
		"use curl -m 5\n" +
		"</skill>\n" +
		"</skills>"
	if out != want {
		t.Errorf("Compose mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestComposeCatalogsOnDemandSkills(t *testing.T) {
	out := Compose("base", []Skill{
		{Name: "lazy", Description: "for lazy things", Body: "LAZY BODY"},
	}, nil)
	want := "base\n\n<available_skills>\n" + catalogIntro + "\n" +
		"<skill name=\"lazy\">for lazy things</skill>\n" +
		"</available_skills>"
	if out != want {
		t.Errorf("Compose mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestComposeLoadedSkillMovesFromCatalogToBodies(t *testing.T) {
	all := []Skill{
		{Name: "alpha", Description: "first", Body: "ALPHA BODY"},
		{Name: "bravo", Description: "second", Body: "BRAVO BODY"},
		{Name: "charlie", Description: "third", Body: "CHARLIE BODY"},
	}
	out := Compose("base", all, []string{"charlie", "alpha"})

	if strings.Contains(out, `<skill name="alpha">first</skill>`) {
		t.Errorf("loaded skill still listed in the catalog:\n%s", out)
	}
	if !strings.Contains(out, `<skill name="bravo">second</skill>`) {
		t.Errorf("unloaded skill missing from the catalog:\n%s", out)
	}
	if strings.Contains(out, "BRAVO BODY") {
		t.Errorf("unloaded skill's body is in the prompt:\n%s", out)
	}
	alpha, charlie := strings.Index(out, "ALPHA BODY"), strings.Index(out, "CHARLIE BODY")
	if alpha < 0 || charlie < 0 {
		t.Fatalf("loaded skills' bodies missing:\n%s", out)
	}
	if alpha > charlie {
		t.Errorf("bodies follow load order rather than skill order:\n%s", out)
	}
	if again := Compose("base", all, []string{"alpha", "charlie"}); again != out {
		t.Errorf("the same set of loaded skills composed two different prompts")
	}
}
