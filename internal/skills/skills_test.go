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

func TestFormatEmpty(t *testing.T) {
	if got := Format(nil); got != "" {
		t.Errorf("Format(nil) = %q, want empty", got)
	}
}

func TestFormatWrapsSkills(t *testing.T) {
	out := Format([]Skill{
		{Name: "general-style", Body: "line one\nline two"},
		{Name: "network-debug", Body: "use curl -m 5"},
	})
	want := "<skills>\n" +
		"<skill name=\"general-style\">\n" +
		"line one\nline two\n" +
		"</skill>\n" +
		"<skill name=\"network-debug\">\n" +
		"use curl -m 5\n" +
		"</skill>\n" +
		"</skills>"
	if out != want {
		t.Errorf("Format mismatch:\n got: %q\nwant: %q", out, want)
	}
	if !strings.HasPrefix(out, "<skills>") || !strings.HasSuffix(out, "</skills>") {
		t.Errorf("Format not wrapped in <skills>: %q", out)
	}
}
