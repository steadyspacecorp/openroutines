package cli

import "testing"

// Color decorates; it never carries. With styling off the string is
// untouched, with it on the words survive inside the escapes.
func TestPaintGatesOnStyled(t *testing.T) {
	was := styled
	defer func() { styled = was }()

	styled = false
	if got := green("✓"); got != "✓" {
		t.Fatalf("unstyled paint altered the string: %q", got)
	}
	styled = true
	if got := green("✓"); got != "\x1b[32m✓\x1b[0m" {
		t.Fatalf("styled paint = %q", got)
	}
}
