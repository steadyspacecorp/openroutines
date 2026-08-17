package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
)

const configureUsage = "usage: openroutines configure [--yes]"

func cmdConfigure(args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--yes": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(configureUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", configureUsage))
	}
	_, yes := flags["--yes"]

	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(err)
	}
	key, store, err := creds.Load(dir)
	if err != nil {
		return fail(err)
	}

	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	in := bufio.NewReader(os.Stdin)
	// eofHit tracks whether stdin ran out mid-prompt: a script piping a
	// complete set of answers reads a real newline for every field and never
	// sets it. An invocation with nothing on stdin hits EOF on the first
	// prompt, and every prompt after would silently take its default --
	// exactly the failure mode that wrote configuration unattended (issue #67).
	eofHit := false
	prompt := func(label, current string) string {
		def := current
		if strings.Contains(def, "{{") {
			def = ""
		}
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, rerr := in.ReadString('\n')
		if rerr != nil {
			eofHit = true
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	agent.Name = prompt("Agent name (required)", agent.Name)
	agent.Owner.Name = prompt("Owner name (optional)", agent.Owner.Name)
	agent.Owner.Email = prompt("Owner email (optional)", agent.Owner.Email)
	agent.Timezone = prompt("Timezone (IANA; required)", defaultTimezone(agent.Timezone))
	agent.Defaults.Model = prompt("Default model (provider/model; required; browse https://models.dev)", agent.Defaults.Model)
	if agent.Defaults.Timeout == "" || strings.Contains(agent.Defaults.Timeout, "{{") {
		agent.Defaults.Timeout = "5m"
	}
	if eofHit && !interactive && !yes {
		return fail(fmt.Errorf("stdin ended before every prompt was answered -- rerun interactively, pipe a complete set of answers, or pass --yes to accept defaults for the rest (nothing was written)"))
	}
	if err := agent.Save(dir); err != nil {
		return fail(err)
	}
	fmt.Printf("Wrote %s\n", config.FileName)

	provider, _, hasProvider := strings.Cut(agent.Defaults.Model, "/")
	providerKey := ""
	if hasProvider && provider != "" {
		providerKey = creds.ProviderKeyName(provider)
	}
	if providerKey != "" {
		if _, ok := store[providerKey]; !ok {
			var value string
			fmt.Printf("%s API key (hidden; enter to skip): ", provider)
			if term.IsTerminal(int(os.Stdin.Fd())) {
				raw, rerr := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Println()
				if rerr != nil {
					return fail(rerr)
				}
				value = strings.TrimSpace(string(raw))
			} else {
				line, _ := in.ReadString('\n')
				value = strings.TrimSpace(line)
			}
			if value != "" {
				store[providerKey] = value
				if err := creds.Write(dir, key, store); err != nil {
					return fail(err)
				}
				fmt.Printf("Added %s\n", providerKey)
			}
		}
	}

	problems := agent.Problems()
	if providerKey != "" {
		if _, ok := store[providerKey]; !ok {
			problems = append(problems, fmt.Sprintf("provider authentication: the default model needs %s (openroutines credentials set %s)", providerKey, providerKey))
		}
	}
	if len(problems) > 0 {
		fmt.Println("\nConfiguration saved. Still needed:")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
	} else {
		fmt.Println("\nAgent configured. Try: openroutines check")
	}
	return 0
}

func defaultTimezone(current string) string {
	if current != "" && !strings.Contains(current, "{{") {
		return current
	}
	if tz, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(tz, "zoneinfo/"); i >= 0 {
			return tz[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}
