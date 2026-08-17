package cli

import "testing"

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
