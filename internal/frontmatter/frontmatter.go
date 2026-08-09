package frontmatter

import (
	"bytes"
	"errors"
)

var (
	ErrMissing      = errors.New("missing frontmatter")
	ErrUnterminated = errors.New("unterminated frontmatter")
)

type Document struct {
	Frontmatter []byte
	Body        []byte

	raw         []byte
	headerStart int
	headerEnd   int
	lineEnding  string
}

// Finds the leading frontmatter block without normalizing source bytes.
func Split(raw []byte) (Document, error) {
	lineEnding := "\n"
	headerStart := len("---\n")
	switch {
	case bytes.HasPrefix(raw, []byte("---\n")):
	case bytes.HasPrefix(raw, []byte("---\r\n")):
		lineEnding = "\r\n"
		headerStart = len("---\r\n")
	default:
		return Document{}, ErrMissing
	}

	for lineStart := headerStart; lineStart <= len(raw); {
		lineEnd := bytes.IndexByte(raw[lineStart:], '\n')
		next := len(raw)
		if lineEnd >= 0 {
			lineEnd += lineStart
			next = lineEnd + 1
		} else {
			lineEnd = len(raw)
		}
		line := bytes.TrimSuffix(raw[lineStart:lineEnd], []byte("\r"))
		if bytes.Equal(line, []byte("---")) {
			headerEnd := lineStart
			if headerEnd > headerStart && raw[headerEnd-1] == '\n' {
				headerEnd--
				if headerEnd > headerStart && raw[headerEnd-1] == '\r' {
					headerEnd--
				}
			}
			return Document{
				Frontmatter: raw[headerStart:headerEnd],
				Body:        raw[next:],
				raw:         raw,
				headerStart: headerStart,
				headerEnd:   headerEnd,
				lineEnding:  lineEnding,
			}, nil
		}
		if next == len(raw) {
			break
		}
		lineStart = next
	}
	return Document{}, ErrUnterminated
}

// Returns the opening delimiter's line ending.
func (d Document) LineEnding() string {
	return d.lineEnding
}

// Replaces only the document's frontmatter bytes.
func (d Document) WithFrontmatter(frontmatter []byte) []byte {
	out := make([]byte, 0, len(d.raw)-len(d.Frontmatter)+len(frontmatter))
	out = append(out, d.raw[:d.headerStart]...)
	out = append(out, frontmatter...)
	if len(d.Frontmatter) == 0 && len(frontmatter) > 0 {
		out = append(out, d.lineEnding...)
	}
	out = append(out, d.raw[d.headerEnd:]...)
	return out
}
