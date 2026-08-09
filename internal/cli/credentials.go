package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// PEM armor markers: a pasted key is read line by line at the hidden prompt
// until its END marker, and a key that begins but never ends is refused
// rather than stored truncated.
const (
	pemBegin = "-----BEGIN"
	pemEnd   = "-----END"
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
	if wantsHelp(args[:1]) {
		fmt.Print(credentialsUsage)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		positional, _, help, err := parseFlags(rest, nil)
		if err != nil {
			return fail(err)
		}
		if help {
			fmt.Println("usage: openroutines credentials list")
			return 0
		}
		if len(positional) != 0 {
			return fail(fmt.Errorf("usage: openroutines credentials list"))
		}
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
		for _, c := range r.Frontmatter.Credentials {
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
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines credentials set <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines credentials set <name>"))
	}
	name := positional[0]
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
		// A pasted PEM arrives one line at a time at the hidden prompt; the
		// first read returns only the BEGIN line, so keep reading until END.
		for strings.HasPrefix(value, pemBegin) && !strings.Contains(value, pemEnd) {
			more, rerr := term.ReadPassword(int(os.Stdin.Fd()))
			if rerr != nil {
				break
			}
			value += "\n" + string(more)
		}
	} else {
		// Piped: the whole stream is the value, so `... set app_key <
		// key.pem` stores the file rather than its first line.
		raw, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			return fail(rerr)
		}
		value = string(raw)
	}
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if value == "" {
		return fail(fmt.Errorf("empty value -- nothing stored"))
	}
	if strings.HasPrefix(value, pemBegin) && !strings.Contains(value, pemEnd) {
		return fail(fmt.Errorf("the value is the beginning of a PEM key without its end -- pipe the whole file: openroutines credentials set %s < key.pem", name))
	}
	// Stored values are one line: exact-string log scrubbing cannot match a
	// value that spans lines. Typed consumers decode the escaping on use.
	multiline := strings.Contains(value, "\n")
	if multiline {
		value = strings.ReplaceAll(value, "\n", `\n`)
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
	if multiline {
		fmt.Printf("Multi-line value stored in the one-line escaped form (\\n between lines), which keeps it scrubbable from logs -- typed credentials decode it on use\n")
	}
	return 0
}

func credentialsRemove(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines credentials remove <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines credentials remove <name>"))
	}
	name := positional[0]
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
		for _, c := range r.Frontmatter.Credentials {
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
