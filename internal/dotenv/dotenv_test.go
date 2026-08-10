package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

// writeEnv writes a .env with the given contents into a fresh temp dir and
// returns the dir.
func writeEnv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestLoadSetsVariables(t *testing.T) {
	dir := writeEnv(t, `
# a comment
OPENROUTER_API_KEY=sk-or-v1-abc123

export EXPORTED=yes
QUOTED="hello world"
SINGLE='raw $value'
SPACED = trimmed
INLINE=value # trailing comment
ESCAPED="line1\nline2"
EQUALS=a=b=c
`)
	for _, k := range []string{"OPENROUTER_API_KEY", "EXPORTED", "QUOTED", "SINGLE", "SPACED", "INLINE", "ESCAPED", "EQUALS"} {
		t.Setenv(k, "") // registered for cleanup; cleared below so Load can set it
		os.Unsetenv(k)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded {
		t.Fatal("Load reported no file, want loaded")
	}

	want := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-v1-abc123",
		"EXPORTED":           "yes",
		"QUOTED":             "hello world",
		"SINGLE":             "raw $value",
		"SPACED":             "trimmed",
		"INLINE":             "value",
		"ESCAPED":            "line1\nline2",
		"EQUALS":             "a=b=c",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadDoesNotOverrideEnvironment(t *testing.T) {
	dir := writeEnv(t, "OPENROUTER_API_KEY=from-file\n")
	t.Setenv("OPENROUTER_API_KEY", "from-environment")

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("OPENROUTER_API_KEY"); got != "from-environment" {
		t.Errorf("API key = %q, want the exported value to win", got)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	loaded, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded {
		t.Error("loaded = true for a directory with no .env")
	}
}

func TestParseLineRejectsJunk(t *testing.T) {
	for _, line := range []string{"", "   ", "# comment", "no_equals_sign", "=novalue", "BAD KEY=1", "1LEADING=digit"} {
		if _, _, ok := parseLine(line); ok {
			t.Errorf("parseLine(%q) accepted, want rejected", line)
		}
	}
}

func TestLoadDefaultUsesWorkingDirectory(t *testing.T) {
	dir := writeEnv(t, "DOTENV_TEST_KEY=wd\n")
	t.Chdir(dir)
	os.Unsetenv("DOTENV_TEST_KEY")

	path, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if path == "" {
		t.Fatal("LoadDefault found no file")
	}
	if got := os.Getenv("DOTENV_TEST_KEY"); got != "wd" {
		t.Errorf("DOTENV_TEST_KEY = %q, want %q", got, "wd")
	}
	os.Unsetenv("DOTENV_TEST_KEY")
}
