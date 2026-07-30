package cli

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

const credentialsUsage = `Manage this agent's encrypted credentials (values never leave the store)

Usage:
  openroutines credentials list           credential names and which routines declare them
  openroutines credentials set <name>     add or replace one value (prompted, hidden)
  openroutines credentials remove <name>  refuses while any routine declares it
`

func cmdCredentials(args []string) int {
	if len(args) == 0 {
		fmt.Print(credentialsUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return credentialsList()
	case "set":
		return credentialsSet(rest)
	case "remove":
		return credentialsRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown credentials command %q\n\n", sub)
		fmt.Print(credentialsUsage)
		return 2
	}
}

func openStore() ([]byte, map[string]string, error) {
	key, err := creds.LoadKey(".")
	if err != nil {
		return nil, nil, err
	}
	store, err := creds.Read(".", key)
	if err != nil {
		return nil, nil, err
	}
	return key, store, nil
}

func credentialsList() int {
	_, store, err := openStore()
	if err != nil {
		return fail(err)
	}
	if len(store) == 0 {
		fmt.Println("No credentials stored. Add one: openroutines credentials set <name>")
		return 0
	}
	declaredBy := map[string][]string{}
	routines, _ := routine.LoadAgent(".")
	for _, r := range routines {
		for _, c := range r.FM.Credentials {
			declaredBy[c] = append(declaredBy[c], r.Name)
		}
	}
	names := make([]string, 0, len(store))
	for name := range store {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		use := "undeclared"
		if holders := declaredBy[name]; len(holders) > 0 {
			use = "declared by " + strings.Join(holders, ", ")
		} else if strings.HasSuffix(name, "_api_key") {
			use = "provider key (auto-injected by model)"
		}
		fmt.Printf("%-24s %s\n", name, use)
	}
	return 0
}

func credentialsSet(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines credentials set <name>"))
	}
	name := args[0]
	if !creds.NamePattern.MatchString(name) {
		return fail(fmt.Errorf("credential name %q must be lowercase snake_case (it becomes the env var %s)", name, strings.ToUpper(name)))
	}
	if strings.HasPrefix(name, creds.ReservedPrefix) {
		return fail(fmt.Errorf("the %s* prefix is reserved for framework metadata", creds.ReservedPrefix))
	}
	if creds.ReservedEnvName(name) {
		return fail(fmt.Errorf("credential name %q would shadow the %s environment variable in every run that declares it", name, strings.ToUpper(name)))
	}
	key, store, err := openStore()
	if err != nil {
		return fail(err)
	}

	var value string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Printf("Value for %s (hidden): ", name)
		raw, rerr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if rerr != nil {
			return fail(rerr)
		}
		value = string(raw)
	} else {
		// Piped: read one line, for scripting (printf 'secret' | ... set name).
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		value = line
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fail(fmt.Errorf("empty value -- nothing stored"))
	}

	_, replacing := store[name]
	store[name] = value
	if err := creds.Write(".", key, store); err != nil {
		return fail(err)
	}
	verb := "Added"
	if replacing {
		verb = "Replaced"
	}
	var spec creds.Spec
	if agent, err := config.Load("."); err == nil {
		spec = agent.Credentials[name]
	}
	fmt.Printf("%s %s %s\n", verb, name, creds.InjectionDescription(name, spec))
	return 0
}

func credentialsRemove(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines credentials remove <name>"))
	}
	name := args[0]
	key, store, err := openStore()
	if err != nil {
		return fail(err)
	}
	if _, ok := store[name]; !ok {
		return fail(fmt.Errorf("no credential %q", name))
	}
	routines, _ := routine.LoadAgent(".")
	var holders []string
	for _, r := range routines {
		for _, c := range r.FM.Credentials {
			if c == name {
				holders = append(holders, r.Name)
			}
		}
	}
	if len(holders) > 0 {
		return fail(fmt.Errorf("credential %q is declared by: %s -- remove those grants first", name, strings.Join(holders, ", ")))
	}
	delete(store, name)
	if err := creds.Write(".", key, store); err != nil {
		return fail(err)
	}
	fmt.Printf("Removed %s\n", name)
	return 0
}
