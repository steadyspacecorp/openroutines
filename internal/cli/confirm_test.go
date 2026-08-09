package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestConfirmDefaultsToNo(t *testing.T) {
	for _, input := range []string{"", "\n", "yes\n", "n\n"} {
		var out bytes.Buffer
		if confirm(bufio.NewReader(strings.NewReader(input)), &out, "Install? ") {
			t.Errorf("confirm(%q) = true, want false", input)
		}
		if out.String() != "Install? [y/N] " {
			t.Errorf("prompt = %q", out.String())
		}
	}
}

func TestConfirmAcceptsY(t *testing.T) {
	if !confirm(bufio.NewReader(strings.NewReader(" Y \n")), &bytes.Buffer{}, "Update? ") {
		t.Fatal("Y should confirm")
	}
}
