package repository

import "testing"

func TestDisplayRedactsURLCredentials(t *testing.T) {
	tests := map[string]string{
		"https://token@example.com/org/repo.git":    "https://example.com/org/repo.git",
		"https://user:secret@example.com/repo.git":  "https://example.com/repo.git",
		"ssh://git:secret@example.com/org/repo.git": "ssh://git@example.com/org/repo.git",
		"git@example.com:org/repo.git":              "git@example.com:org/repo.git",
	}
	for input, want := range tests {
		if got := Display(input); got != want {
			t.Errorf("Display(%q) = %q, want %q", input, got, want)
		}
	}
}
