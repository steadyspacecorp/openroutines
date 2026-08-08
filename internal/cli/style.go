package cli

import "os"

// Terminal styling for command output: stdlib ANSI escapes, decided once at
// load. Color is never load-bearing -- it decorates words and marks that
// already carry the information (active, inactive, ✓, ✗), so a pipe, a CI
// log, or NO_COLOR loses nothing but emphasis. This is the whole styling
// layer on purpose: the supervisor is the trusted component, and a styling
// dependency tree is attack surface spent on cosmetics (design decision
// "CLI color: stdlib ANSI, never load-bearing").
var styled = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}()

func paint(code, s string) string {
	if !styled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string  { return paint("1", s) }
func dim(s string) string   { return paint("2", s) }
func green(s string) string { return paint("32", s) }
func red(s string) string   { return paint("31", s) }

// warnMark is the one yellow thing in the output; precomputing it keeps
// the palette honest about that.
var warnMark = paint("33", "!")
