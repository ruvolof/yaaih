// Package frontmatter separates a Markdown document's YAML frontmatter from
// its body. The frontmatter opens on the document's first line with a run of
// three or more '-' and closes at the next line carrying the same run.
package frontmatter

import (
	"bytes"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// ErrMissing is returned for a document whose first line is not a
// frontmatter delimiter.
var ErrMissing = errors.New("missing YAML frontmatter")

// Parse decodes the frontmatter of document into out and returns the Markdown
// body that follows its closing delimiter.
func Parse(document []byte, out any) (body string, err error) {
	firstLine, _, _ := bytes.Cut(document, []byte("\n"))
	delimiter := trimLineEnd(firstLine)
	if !isDelimiter(delimiter) {
		return "", ErrMissing
	}

	yamlStart := min(len(firstLine)+1, len(document))
	for lineStart := yamlStart; lineStart < len(document); {
		line, _, _ := bytes.Cut(document[lineStart:], []byte("\n"))
		nextLine := min(lineStart+len(line)+1, len(document))
		if bytes.Equal(trimLineEnd(line), delimiter) {
			if err := yaml.Unmarshal(document[yamlStart:lineStart], out); err != nil {
				return "", fmt.Errorf("decode frontmatter: %w", err)
			}
			return string(document[nextLine:]), nil
		}
		lineStart = nextLine
	}
	return "", fmt.Errorf("frontmatter opened with %q is never closed", delimiter)
}

func isDelimiter(line []byte) bool {
	return len(line) >= 3 && len(bytes.Trim(line, "-")) == 0
}

// trimLineEnd drops trailing blanks, including the '\r' of a CRLF file.
func trimLineEnd(line []byte) []byte {
	return bytes.TrimRight(line, " \t\r")
}
