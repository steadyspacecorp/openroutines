//go:build linux

package sandbox

import (
	"os"
	"strings"
	"testing"
)

func TestStatusIdentity(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		uid     uint32
		matches bool
		zombie  bool
	}{
		{"effective uid", "State:\tS (sleeping)\nUid:\t10001\t20000\t10001\t10001\n", 20000, true, false},
		{"different uid", "State:\tS (sleeping)\nUid:\t10001\t10001\t10001\t10001\n", 20000, false, false},
		{"zombie", "State:\tZ (zombie)\nUid:\t20000\t20000\t20000\t20000\n", 20000, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "status")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(tt.status); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			matches, zombie, err := statusIdentity(file, tt.uid)
			if err != nil {
				t.Fatal(err)
			}
			if matches != tt.matches || zombie != tt.zombie {
				t.Fatalf("matches=%v zombie=%v, want %v %v (%s)", matches, zombie, tt.matches, tt.zombie, strings.TrimSpace(tt.status))
			}
		})
	}
}
