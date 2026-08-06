package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

const memoryUsage = "usage: openroutines memory [--no-sync] [--tasks] [--events] [--context] [--ledger <routine>] [--json]"

type memoryView struct {
	Tasks       string            `json:"tasks,omitempty"`
	Events      string            `json:"events,omitempty"`
	Context     string            `json:"context,omitempty"`
	Ledgers     map[string]string `json:"ledgers,omitempty"`
	showTasks   bool
	showEvents  bool
	showContext bool
}

func cmdMemory(args []string) int {
	positional, flags, help, err := parseMemoryFlags(args)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(memoryUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", memoryUsage))
	}

	mem := memory.At(".")
	_, noSync := flags["--no-sync"]
	if noSync && !mem.Status().Materialized {
		return fail(errors.New("memory is not materialized in this checkout; run openroutines memory without --no-sync to fetch it"))
	}
	if err := mem.Ensure(); err != nil {
		return fail(err)
	}
	_, jsonOutput := flags["--json"]
	if !noSync {
		message, err := syncMemoryForRead(mem)
		if err != nil {
			return fail(err)
		}
		if !jsonOutput {
			fmt.Printf("%s\n\n", message)
		}
	}

	view, err := loadMemoryView(mem.Worktree(), flags)
	if err != nil {
		return fail(err)
	}
	if jsonOutput {
		out, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(out))
		return 0
	}
	renderMemoryView(view)
	return 0
}

// parseMemoryFlags accepts repeatable --ledger values; the shared flag parser
// intentionally rejects duplicates for every other command.
func parseMemoryFlags(args []string) ([]string, map[string]string, bool, error) {
	if wantsHelp(args) {
		return nil, nil, true, nil
	}
	flags := map[string]string{}
	var positional, ledgers []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ledger":
			if i+1 >= len(args) {
				return nil, nil, false, errors.New("flag --ledger requires a value")
			}
			i++
			ledgers = append(ledgers, args[i])
		case "--no-sync", "--tasks", "--events", "--context", "--json":
			if _, exists := flags[args[i]]; exists {
				return nil, nil, false, fmt.Errorf("flag %s given more than once", args[i])
			}
			flags[args[i]] = ""
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, nil, false, fmt.Errorf("unknown flag %s", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	flags["--ledger"] = strings.Join(ledgers, "\x00")
	return positional, flags, false, nil
}

func syncMemoryForRead(mem *memory.Memory) (string, error) {
	rep := mem.Sync()
	switch {
	case rep.NoOrigin:
		return "Memory is local only (no origin)", nil
	case rep.RemoteMissing:
		return fmt.Sprintf("Memory is local only (origin has no %s branch yet)", memory.Branch), nil
	case rep.Unreachable:
		return "", fmt.Errorf("origin unreachable: %s", rep.Detail)
	case rep.Rewritten:
		reportStranded(mem)
		return "", errors.New(rep.Detail)
	case rep.Conflict:
		reportStranded(mem)
		return "", fmt.Errorf("memory does not reconcile cleanly: %s\n\nresolve inside memory/, commit, then rerun openroutines memory", rep.Detail)
	case rep.Detail != "":
		return "", errors.New(rep.Detail)
	default:
		return fmt.Sprintf("Memory is current with origin/%s", memory.Branch), nil
	}
}

func loadMemoryView(worktree string, flags map[string]string) (memoryView, error) {
	_, tasks := flags["--tasks"]
	_, events := flags["--events"]
	_, context := flags["--context"]
	ledgerArg := flags["--ledger"]
	selected := tasks || events || context || ledgerArg != ""
	view := memoryView{}
	var err error
	if tasks || !selected {
		view.showTasks = true
		view.Tasks, err = readMemoryRecords(filepath.Join(worktree, "tasks.md"))
	}
	if err == nil && (events || !selected) {
		view.showEvents = true
		view.Events, err = readMemoryRecords(filepath.Join(worktree, "events.md"))
	}
	if err == nil && (context || !selected) {
		view.showContext = true
		view.Context, err = readMemoryRecords(filepath.Join(worktree, "context.md"))
	}
	if err != nil {
		return view, err
	}
	if ledgerArg != "" {
		for _, name := range strings.Split(ledgerArg, "\x00") {
			if filepath.Base(name) != name || name == "." || name == ".." {
				return view, fmt.Errorf("invalid routine name %q", name)
			}
			if !strings.HasSuffix(name, ".md") {
				name += ".md"
			}
			if err := addLedger(&view, worktree, name, true); err != nil {
				return view, err
			}
		}
	} else if !selected {
		entries, err := os.ReadDir(filepath.Join(worktree, "ledgers"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return view, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				if err := addLedger(&view, worktree, entry.Name(), false); err != nil {
					return view, err
				}
			}
		}
	}
	return view, nil
}

func addLedger(view *memoryView, worktree, name string, required bool) error {
	content, err := readMemoryRecords(filepath.Join(worktree, "ledgers", name))
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading ledger %s: %w", name, err)
	}
	if view.Ledgers == nil {
		view.Ledgers = map[string]string{}
	}
	view.Ledgers[strings.TrimSuffix(name, ".md")] = content
	return nil
}

func readMemoryRecords(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(raw)
	if start := strings.Index(content, "```"); start >= 0 {
		// Only the seeded teaching block is presentation scaffolding. A ledger
		// or curated primitive may legitimately contain a fenced code block,
		// which must remain part of the record.
		if end := strings.Index(content[start+3:], "```"); strings.Contains(content[:start], "Format") && end >= 0 {
			content = content[start+3+end+3:]
		}
	}
	return strings.TrimSpace(content), nil
}

func renderMemoryView(view memoryView) {
	if view.showTasks {
		renderMemorySection("Tasks", view.Tasks)
	}
	if view.showEvents {
		renderMemorySection("Recent events", view.Events)
	}
	if view.showContext {
		renderMemorySection("Context", view.Context)
	}
	if view.Ledgers == nil {
		return
	}
	fmt.Println("Routine state")
	names := make([]string, 0, len(view.Ledgers))
	for name := range view.Ledgers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("\n## %s\n\n%s\n", name, emptyMemory(view.Ledgers[name]))
	}
}

func renderMemorySection(title, content string) {
	fmt.Printf("%s\n\n%s\n\n", title, emptyMemory(content))
}

func emptyMemory(content string) string {
	if content == "" {
		return "(none)"
	}
	return content
}
