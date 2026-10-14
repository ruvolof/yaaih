package frontmatter

import (
	"errors"
	"strings"
	"testing"
)

type header struct {
	Name   string   `yaml:"name"`
	Tags   []string `yaml:"tags"`
	Always bool     `yaml:"always"`
}

func TestParseSplitsFrontmatterFromBody(t *testing.T) {
	document := "---\nname: helper\ntags:\n  - a\n  - b\n---\n# Title\n\nBody text.\n"

	var parsed header
	body, err := Parse([]byte(document), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "helper" || len(parsed.Tags) != 2 || parsed.Tags[1] != "b" {
		t.Errorf("frontmatter = %+v, want name helper and tags [a b]", parsed)
	}
	if body != "# Title\n\nBody text.\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseAcceptsCRLF(t *testing.T) {
	document := "---\r\nname: windows\r\n---\r\nBody\r\n"

	var parsed header
	body, err := Parse([]byte(document), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "windows" {
		t.Errorf("name = %q, want windows", parsed.Name)
	}
	if body != "Body\r\n" {
		t.Errorf("body = %q, want %q", body, "Body\r\n")
	}
}

// A horizontal rule in the body is a line of dashes too; only the first line
// carrying the opening run closes the frontmatter.
func TestParseStopsAtTheFirstMatchingDelimiter(t *testing.T) {
	document := "---\nname: first\n---\nabove\n\n---\n\nbelow\n"

	var parsed header
	body, err := Parse([]byte(document), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if body != "above\n\n---\n\nbelow\n" {
		t.Errorf("body = %q", body)
	}
}

func TestParseClosesOnlyOnTheSameRunOfDashes(t *testing.T) {
	document := "-----\nname: long\n---\n-----\nbody\n"

	var parsed header
	body, err := Parse([]byte(document), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "long" || body != "body\n" {
		t.Errorf("got name %q body %q", parsed.Name, body)
	}
}

func TestParseWithoutBody(t *testing.T) {
	var parsed header
	body, err := Parse([]byte("---\nname: bare\n---"), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "bare" || body != "" {
		t.Errorf("got name %q body %q", parsed.Name, body)
	}
}

func TestParseEmptyFrontmatterLeavesTargetUntouched(t *testing.T) {
	parsed := header{Name: "preset"}
	body, err := Parse([]byte("---\n---\nbody\n"), &parsed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Name != "preset" || body != "body\n" {
		t.Errorf("got name %q body %q", parsed.Name, body)
	}
}

func TestParseMissingFrontmatter(t *testing.T) {
	for _, document := range []string{
		"",
		"just some text\n",
		"--\nname: short\n--\n",
		"\n---\nname: late\n---\n",
		"--- name: inline\n---\n",
	} {
		var parsed header
		if _, err := Parse([]byte(document), &parsed); !errors.Is(err, ErrMissing) {
			t.Errorf("Parse(%q) error = %v, want ErrMissing", document, err)
		}
	}
}

func TestParseUnclosedFrontmatter(t *testing.T) {
	var parsed header
	_, err := Parse([]byte("---\nname: open\nbody without a closing line\n"), &parsed)
	if err == nil || errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "never closed") {
		t.Errorf("error = %v, want a never-closed error", err)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	var parsed header
	_, err := Parse([]byte("---\nname: [unterminated\n---\nbody\n"), &parsed)
	if err == nil || !strings.Contains(err.Error(), "decode frontmatter") {
		t.Errorf("error = %v, want a decode error", err)
	}
}
