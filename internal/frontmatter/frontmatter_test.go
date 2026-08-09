package frontmatter

import (
	"errors"
	"testing"
)

func TestSplitAndReplace(t *testing.T) {
	raw := []byte("---\nname: one\n---\nbody\n---\n")
	doc, err := Split(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(doc.Frontmatter); got != "name: one" {
		t.Fatalf("frontmatter = %q", got)
	}
	if got := string(doc.Body); got != "body\n---\n" {
		t.Fatalf("body = %q", got)
	}
	if got := string(doc.WithFrontmatter([]byte("name: two"))); got != "---\nname: two\n---\nbody\n---\n" {
		t.Fatalf("WithFrontmatter() = %q", got)
	}
}

func TestSplitAcceptsCRLFAndClosingDelimiterAtEOF(t *testing.T) {
	doc, err := Split([]byte("---\r\nname: one\r\n---"))
	if err != nil {
		t.Fatal(err)
	}
	if string(doc.Frontmatter) != "name: one" || len(doc.Body) != 0 || doc.LineEnding() != "\r\n" {
		t.Fatalf("Split() = %#v", doc)
	}
}

func TestReplaceEmptyFrontmatter(t *testing.T) {
	doc, err := Split([]byte("---\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(doc.WithFrontmatter([]byte("name: one"))); got != "---\nname: one\n---\nbody" {
		t.Fatalf("WithFrontmatter() = %q", got)
	}
}

func TestSplitErrors(t *testing.T) {
	if _, err := Split([]byte("name: one\n")); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing delimiter error = %v", err)
	}
	if _, err := Split([]byte("---\nname: one\n")); !errors.Is(err, ErrUnterminated) {
		t.Fatalf("unterminated delimiter error = %v", err)
	}
}
