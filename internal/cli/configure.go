package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
)

// cmdConfigure interactively fills in agent.yaml, generates the master key,
// and seeds encrypted credentials. Idempotent: existing values are defaults.
func cmdConfigure(args []string) int {
	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(fmt.Errorf("not an agent repository (no %s): %w", config.FileName, err))
	}

	in := bufio.NewReader(os.Stdin)
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
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	agent.Name = prompt("Agent name", agent.Name)
	agent.Description = prompt("Job description", strings.TrimSpace(agent.Description))
	agent.Owner.Name = prompt("Owner name", agent.Owner.Name)
	agent.Owner.Email = prompt("Owner email", agent.Owner.Email)
	agent.Timezone = prompt("Timezone (IANA)", defaultTimezone(agent.Timezone))
	agent.Defaults.Model = prompt("Default model (provider/model)", defaultModel(agent.Defaults.Model))
	if agent.Defaults.Timeout == "" || strings.Contains(agent.Defaults.Timeout, "{{") {
		agent.Defaults.Timeout = "5m"
	}
	if err := agent.Save(dir); err != nil {
		return fail(err)
	}
	fmt.Printf("Wrote %s\n", config.FileName)

	// Master key: generate once, never overwrite.
	keyPath := filepath.Join(dir, creds.KeyFileName)
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		if err := os.WriteFile(keyPath, []byte(creds.GenerateKey()+"\n"), 0o600); err != nil {
			return fail(err)
		}
		fmt.Printf("Generated %s (gitignored -- in production, mount it read-only and point %s at the path)\n", creds.KeyFileName, creds.EnvMasterKeyFile)
	} else {
		fmt.Printf("Master key already present (%s)\n", creds.KeyFileName)
	}

	key, err := creds.LoadKey(dir)
	if err != nil {
		return fail(err)
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		return fail(err)
	}

	// Offer to store the provider key for the default model.
	provider := strings.SplitN(agent.Defaults.Model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)
	if _, ok := store[providerKey]; !ok {
		val := prompt(fmt.Sprintf("%s API key (enter to skip)", provider), "")
		if val != "" {
			store[providerKey] = val
		}
	}
	if err := creds.Write(dir, key, store); err != nil {
		return fail(err)
	}
	fmt.Printf("Wrote %s (%d credential(s))\n", creds.FileName, len(store))

	// Report what's still missing rather than failing -- and never call the
	// agent configured while its model has no way to authenticate: the
	// first-run failure that causes is opaque (an opencode server error),
	// and in production it burns retry attempts before anyone learns why.
	problems := agent.Problems()
	if _, ok := store[providerKey]; !ok {
		problems = append(problems, fmt.Sprintf("provider authentication: the default model needs %s (openroutines credentials set %s)", providerKey, providerKey))
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

func defaultModel(current string) string {
	if current != "" && !strings.Contains(current, "{{") {
		return current
	}
	return "anthropic/claude-sonnet-5"
}
