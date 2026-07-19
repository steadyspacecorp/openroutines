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
		return fail(fmt.Errorf("usage: openroutines skills <new|list|remove> [name]"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return skillsNew(rest)
	case "list":
		return skillsList()
	case "remove":
		return skillsRemove(rest)
	default:
		return fail(fmt.Errorf("unknown skills command %q", sub))
	}
}

func skillsNew(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines skills new <name>"))
	}
	name := args[0]
	if !skill.NamePattern.MatchString(name) {
		return fail(fmt.Errorf("skill name %q must be lowercase alphanumerics and hyphens (agentskills.io format)", name))
	}
	dir := filepath.Join("skills", name)
	path := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(path); err == nil {
		return fail(fmt.Errorf("%s already exists", path))
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
	skills, errs := skill.List("skills")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(skills) == 0 {
		fmt.Println("No skills. Create one (openroutines skills new <name>) or vendor one in from the ecosystem (agentskills.io).")
		return 0
	}
	// Show which routines use each skill: grants are the interesting part.
	usedBy := map[string][]string{}
	routines, _ := routine.LoadDir("routines")
	for _, r := range routines {
		for _, s := range r.FM.Skills {
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
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines skills remove <name>"))
	}
	name := args[0]
	dir := filepath.Join("skills", name)
	if _, err := os.Stat(dir); err != nil {
		return fail(fmt.Errorf("no skill %q", name))
	}
	// Refuse while any routine still declares the grant.
	routines, _ := routine.LoadDir("routines")
	var holders []string
	for _, r := range routines {
		for _, s := range r.FM.Skills {
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
