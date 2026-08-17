package cli

import (
	"fmt"
	"strings"
)

type flagSpec struct {
	value bool
}

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
			flags[a] = ""
		}
	}
	return positional, flags, false, nil
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}
