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

// frameworkOwned are the template files update may rewrite. Everything else
// in an agent repo -- routines, skills, memory, agent.yaml, credentials --
// belongs to the agent and is never touched. opencode.json stays off this
// list deliberately: it is the user's harness config (the permission policy
// and provider endpoint definitions; DESIGN.md "One binary"), and an update
// that rewrote it would clobber both -- bin/smoke asserts it survives an
// update byte for byte.
var frameworkOwned = []string{
	"Dockerfile",
	".dockerignore",
	".gitignore",
	"AGENTS.md",
}

// cmdUpdate brings the agent up to the version of the running binary: bumps
// the pin and offers each framework-owned file's changes interactively
// (rails app:update style). It stages nothing -- review, commit, push.
func cmdUpdate(args []string) int {
	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(fmt.Errorf("not an agent repository (no %s)", config.FileName))
	}

	target := version.Version
	pinPath := filepath.Join(dir, ".openroutines-version")
	current := "(none)"
	if raw, err := os.ReadFile(pinPath); err == nil {
		current = strings.TrimSpace(string(raw))
	}
	if current == target {
		fmt.Printf("Already at %s. (update targets the version of the openroutines binary you run -- install a newer binary first.)\n", target)
		return 0
	}
	if target == "v0.0.0-dev" {
		fmt.Println("warning: this is a development build of openroutines -- updating the agent to an unreleased version")
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
			// Say so: silence here reads as "this file isn't managed".
			fmt.Printf("%s: already current\n", name)
			continue
		}
		fmt.Printf("--- %s differs from the %s template ---\n", name, target)
		printDiff(string(gotRaw), want)
		apply := true
		// The Dockerfile's base image and .openroutines-version are one pin.
		// Letting a user skip one while advancing the other produces an agent
		// whose local/CI framework version differs from production.
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
		return fail(fmt.Errorf("Dockerfile still uses a different openroutines base image -- .openroutines-version was left at %s; update the FROM tag to %s and rerun", current, target))
	}
	if err := os.WriteFile(pinPath, []byte(target+"\n"), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Pinned %s in .openroutines-version (%d file(s) updated, %d skipped)\n\n", target, applied, skipped)
	fmt.Println("Review the diff, commit, and push -- your next deploy runs the new version.")
	fmt.Println("Rolling back is git revert. Routines, skills, memory, and credentials were not touched.")
	return 0
}

func dockerfileUsesVersion(raw []byte, version string) bool {
	want := "ghcr.io/steadyspacecorp/openroutines:" + version
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") && fields[1] == want {
			return true
		}
	}
	return false
}

// printDiff shows a compact line diff: removed lines from ours, added lines
// from the template. Good enough to review; git diff gives the full view.
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
