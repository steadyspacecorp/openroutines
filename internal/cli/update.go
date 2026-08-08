package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	openroutines "github.com/steadyspacecorp/openroutines"
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

// The template files update may rewrite. Everything else in an agent repo --
// routines, skills, knowledge, openroutines.yml, credentials -- belongs to
// the agent and is never touched. opencode.json stays off this list
// deliberately: it's the user's harness config (design decision "One binary"),
// and bin/smoke asserts it survives an update byte for byte.
var frameworkOwned = []string{
	"Dockerfile",
	".dockerignore",
	".gitignore",
	"AGENTS.md",
}

const updateUsage = "usage: openroutines update"

// Brings the agent up to the version of the running binary: bumps the pin
// and offers each framework-owned file's changes interactively, rails
// app:update style. Stages nothing -- review, commit, push.
func cmdUpdate(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(updateUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", updateUsage))
	}

	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(err)
	}

	target := version.Version
	pinPath := filepath.Join(dir, ".openroutines", "version")
	current := "(none)"
	if raw, err := os.ReadFile(pinPath); err == nil {
		current = strings.TrimSpace(string(raw))
	}
	if current == target {
		fmt.Printf("Already at %s. (update targets the version of the openroutines binary you run -- install a newer binary first.)\n", target)
		return 0
	}
	if target == "v0.0.0-dev" {
		fmt.Println("warning: this is a development build of OpenRoutines -- updating the agent to an unreleased version")
	}
	fmt.Printf("Updating agent from %s to %s\n\n", current, target)

	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	in := bufio.NewReader(os.Stdin)
	renderer := strings.NewReplacer(
		"{{AGENT_NAME}}", agent.Name,
		"{{OPENROUTINES_VERSION}}", target,
	)

	applied, skipped := 0, 0
	for _, name := range frameworkOwned {
		tmpl, err := openroutines.TemplateFS.ReadFile(templateRoot + "/" + name)
		if err != nil {
			continue // not every framework file exists in every template version
		}
		want := renderer.Replace(string(tmpl))
		gotRaw, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && string(gotRaw) == want {
			fmt.Printf("%s: already current\n", name)
			continue
		}
		fmt.Printf("--- %s differs from the %s template ---\n", name, target)
		printDiff(string(gotRaw), want)
		apply := true
		// The Dockerfile's version ARG and .openroutines/version are one pin;
		// skipping one while advancing the other splits local/CI from production.
		if interactive && name != "Dockerfile" {
			fmt.Printf("Apply update to %s? [Y/n] ", name)
			line, _ := in.ReadString('\n')
			apply = strings.TrimSpace(strings.ToLower(line)) != "n"
		} else if interactive {
			fmt.Println("applying Dockerfile update (required to keep the deploy image aligned with the framework pin)")
		}
		if !apply {
			skipped++
			fmt.Printf("skipped %s (yours kept as-is)\n\n", name)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(want), 0o644); err != nil {
			return fail(err)
		}
		applied++
		fmt.Printf("updated %s\n\n", name)
	}

	dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return fail(fmt.Errorf("cannot verify Dockerfile framework version: %w", err))
	}
	if !dockerfileUsesVersion(dockerfile, target) {
		return fail(fmt.Errorf("the Dockerfile still pins a different openroutines version -- .openroutines/version was left at %s; set ARG OPENROUTINES_VERSION=%s and rerun", current, target))
	}
	if err := os.WriteFile(pinPath, []byte(target+"\n"), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Pinned %s in .openroutines/version (%d file(s) updated, %d skipped)\n\n", target, applied, skipped)
	fmt.Println("Review the diff, commit, and push -- your next deploy runs the new version.")
	fmt.Println("Rolling back is git revert. Routines, skills, knowledge, and credentials were not touched.")
	return 0
}

func dockerfileUsesVersion(raw []byte, version string) bool {
	want := "ARG OPENROUTINES_VERSION=" + version
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// Shows a compact line diff; git diff gives the full view.
func printDiff(got, want string) {
	gotLines := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		gotLines[l] = true
	}
	wantLines := map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		wantLines[l] = true
	}
	shown := 0
	for _, l := range strings.Split(got, "\n") {
		if !wantLines[l] && strings.TrimSpace(l) != "" {
			fmt.Printf("  - %s\n", l)
			if shown++; shown > 20 {
				fmt.Println("  ... (more; see git diff after applying)")
				break
			}
		}
	}
	for _, l := range strings.Split(want, "\n") {
		if !gotLines[l] && strings.TrimSpace(l) != "" {
			fmt.Printf("  + %s\n", l)
			if shown++; shown > 40 {
				fmt.Println("  ... (more; see git diff after applying)")
				break
			}
		}
	}
}
