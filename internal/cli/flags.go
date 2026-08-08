package cli

import (
	"fmt"
	"strings"
)

// Says whether a recognized flag consumes the following argument as its
// value (--path sub/dir) or is a bare switch (--yes).
type flagSpec struct {
	value bool
}

// Splits args into positional arguments and recognized flags, rejecting
// anything unrecognized that looks like a flag rather than silently
// dropping it. --help/-h are checked before any other validation, so
// `cmd --bogus --help` still shows help instead of failing.
func parseFlags(args []string, known map[string]flagSpec) (positional []string, flags map[string]string, help bool, err error) {
	if wantsHelp(args) {
		return nil, nil, true, nil
	}
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		spec, ok := known[a]
		if !ok {
			return nil, nil, false, fmt.Errorf("unknown flag %s", a)
		}
		if _, dup := flags[a]; dup {
			return nil, nil, false, fmt.Errorf("flag %s given more than once", a)
		}
		if spec.value {
			if i+1 >= len(args) {
				return nil, nil, false, fmt.Errorf("flag %s requires a value", a)
			}
			i++
			flags[a] = args[i]
		} else {
			// A bare switch's fact is its presence -- read it with
			// `_, ok := flags["--yes"]`, never by comparing the value.
			flags[a] = ""
		}
	}
	return positional, flags, false, nil
}

// Reports whether args ask for help via --help or -h, wherever it appears --
// so `check --help` shows what check accepts instead of running the check.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}
