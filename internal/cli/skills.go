package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

const newSkillTemplate = `---
name: %s
description: TODO -- one line saying what this skill does and when to use it
---

Instructions for the agent. Explain the workflow this skill enables, any
scripts or references bundled alongside this file, and what a good result
looks like. Routines only get this skill if their frontmatter declares it.
`

func cmdSkills(args []string) int {
	if len(args) == 0 {
		fmt.Print(skillsUsage)
		return 2
	}
	if wantsHelp(args[:1]) {
		fmt.Print(skillsUsage)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return skillsNew(rest)
	case "add":
		return skillsAdd(rest)
	case "list":
		positional, _, help, err := parseFlags(rest, nil)
		if err != nil {
			return fail(err)
		}
		if help {
			fmt.Println("usage: openroutines skills list")
			return 0
		}
		if len(positional) != 0 {
			return fail(fmt.Errorf("usage: openroutines skills list"))
		}
		return skillsList()
	case "remove":
		return skillsRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown skills command %q\n\n", sub)
		fmt.Print(skillsUsage)
		return 2
	}
}

const skillsUsage = `Manage this agent's skills (Agent Skills format -- agentskills.io)

Usage:
  openroutines skills new <name>           scaffold a blank skill
  openroutines skills new <git-url | owner/repo> [--path <sub/dir>]
                                           vendor a skill from a git repository
  openroutines skills list                 skills and which routines use them
  openroutines skills remove <name>        refuses while any routine declares it
`

const skillsNewUsage = "usage: openroutines skills new <name | git-url | owner/repo> [--path <sub/dir>]"

func skillsNew(args []string) int {
	if wantsHelp(args) {
		fmt.Println(skillsNewUsage)
		return 0
	}
	if len(args) == 0 {
		return fail(fmt.Errorf("%s", skillsNewUsage))
	}
	// A skill name can't contain / : or @ -- so an argument that does is a
	// source to vendor from, not a name to scaffold.
	if strings.ContainsAny(args[0], "/:@") {
		return skillsAdd(args)
	}
	if len(args) != 1 {
		return fail(fmt.Errorf("%s", skillsNewUsage))
	}
	name := args[0]
	if !skill.NamePattern.MatchString(name) {
		return fail(fmt.Errorf("skill name %q must be lowercase alphanumerics and hyphens (agentskills.io format)", name))
	}
	dir := filepath.Join("skills", name)
	path := filepath.Join(dir, "SKILL.md")
	if existing, err := skill.Find(".", name); err == nil {
		return fail(fmt.Errorf("skill %q already exists at %s", name, existing.Dir))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, newSkillTemplate, name), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Created %s -- edit the description and instructions, then grant it to a routine via its frontmatter\n", path)
	return 0
}

func skillsList() int {
	skills, errs := skill.ListAgent(".")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(skills) == 0 {
		fmt.Println("No skills. Create one (openroutines skills new <name>) or vendor one in from the ecosystem (agentskills.io).")
		return 0
	}
	usedBy := map[string][]string{}
	routines, _ := routine.LoadAgent(".")
	for _, r := range routines {
		for _, s := range r.Frontmatter.Skills {
			usedBy[s] = append(usedBy[s], r.Name)
		}
	}
	for _, s := range skills {
		use := "unused"
		if names := usedBy[s.Name]; len(names) > 0 {
			use = "used by " + strings.Join(names, ", ")
		}
		fmt.Printf("%-20s %s (%s)\n", s.Name, firstLine(s.Description), use)
	}
	return 0
}

func skillsRemove(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines skills remove <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines skills remove <name>"))
	}
	name := positional[0]
	// Grammar first, then containment -- this path ends in os.RemoveAll,
	// and an unvalidated name like "../.." would resolve outside skills/.
	if !skill.NamePattern.MatchString(name) {
		return fail(fmt.Errorf("skill name %q is not a valid Agent Skills name", name))
	}
	dir := filepath.Join("skills", name)
	if rel, err := filepath.Rel("skills", dir); err != nil || rel != name {
		return fail(fmt.Errorf("skill name %q escapes skills/", name))
	}
	if _, err := os.Stat(dir); err != nil {
		return fail(fmt.Errorf("no skill %q", name))
	}
	routines, _ := routine.LoadAgent(".")
	var holders []string
	for _, r := range routines {
		for _, s := range r.Frontmatter.Skills {
			if s == name {
				holders = append(holders, r.Name)
			}
		}
	}
	if len(holders) > 0 {
		return fail(fmt.Errorf("skill %q is declared by: %s -- remove those grants first", name, strings.Join(holders, ", ")))
	}
	if err := os.RemoveAll(dir); err != nil {
		return fail(err)
	}
	fmt.Printf("Removed %s\n", dir)
	return 0
}
