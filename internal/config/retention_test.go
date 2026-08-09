package config

import (
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", DefaultRetention, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"0d", 0, true},
		{"abc", 0, true},
		{"-5d", 0, true},
	} {
		got, err := ParseRetention(tc.in)
		if tc.err != (err != nil) || (!tc.err && got != tc.want) {
			t.Fatalf("ParseRetention(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.err)
		}
	}
}
