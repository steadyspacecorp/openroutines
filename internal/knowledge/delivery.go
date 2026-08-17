package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Cursor struct {
	ConsumedThrough string    `json:"consumed_through"`
	ByRun           string    `json:"by_run"`
	At              time.Time `json:"at"`
}

func CursorFile(consumer string) string {
	return path.Join(stateDirName, "cursors", consumer+".json")
}

func (store *Store) cursorPath(consumer string) string {
	return filepath.Join(store.Worktree(), CursorFile(consumer))
}

func (store *Store) LoadCursor(consumer string) (*Cursor, error) {
	raw, err := os.ReadFile(store.cursorPath(consumer))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("cursor %s: %w", consumer, err)
	}

	// The cursor value becomes a git rev-range argv element, and cursor
	// files live on the knowledge branch -- an untrusted, human-writable
	// channel. Only a commit SHA is acceptable.
	if !shaPattern.MatchString(c.ConsumedThrough) {
		return nil, fmt.Errorf("cursor %s: consumed_through %q is not a commit SHA", consumer, c.ConsumedThrough)
	}
	return &c, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func (store *Store) SaveCursor(consumer string, c Cursor) error {
	p := store.cursorPath(consumer)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}

func (store *Store) Cursors() (map[string]Cursor, error) {
	dir := filepath.Join(store.StateDir(), "cursors")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Cursor{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if c, err := store.LoadCursor(name); err == nil && c != nil {
			out[name] = *c
		}
	}
	return out, nil
}

func (store *Store) Head() (string, error) {
	return store.worktree.Run("rev-parse", "HEAD")
}

type CommitChange struct {
	SHA     string
	Date    string
	Subject string
	Files   []FileDelta
}

type FileDelta struct {
	Path    string
	Added   []string
	Removed []string
}

var deliveryExcludes = []string{":(exclude)state", ":(exclude)runs.jsonl", ":(exclude)ledgers"}

var ErrCursorUnreachable = errors.New("consumer cursor is not on the knowledge branch")

func (store *Store) reachable(from, through string) error {
	full, exists, err := store.worktree.ResolveCommit(from)
	if err != nil {
		return fmt.Errorf("delivery changes: reading cursor commit: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: commit %.12s is not in this repository", ErrCursorUnreachable, from)
	}
	ancestor, err := store.worktree.IsAncestor(full, through)
	if err != nil {
		return fmt.Errorf("delivery changes: relating cursor to boundary: %w", err)
	}
	if !ancestor {
		return fmt.Errorf("%w: commit %.12s is not an ancestor of %.12s", ErrCursorUnreachable, from, through)
	}
	return nil
}

func (store *Store) Changes(from, through string) ([]CommitChange, error) {
	if from == "" || through == "" {
		return nil, fmt.Errorf("delivery changes: empty commit range")
	}
	if err := store.reachable(from, through); err != nil {
		return nil, err
	}

	args := append([]string{
		"log", "--reverse", "--date=format:%Y-%m-%d",
		"--format=%x00%H%x1f%ad%x1f%s", "-p", "-U0", "--no-color",
		"--invert-grep", "--grep=^" + trimTrailer + "$",
		from + ".." + through, "--", ".",
	}, deliveryExcludes...)
	// NUL and unit-separator sentinels are emitted by git because argv cannot
	// carry a literal NUL; trim commits are excluded so consumed history is not replayed.
	out, err := store.worktree.Run(args...)
	if err != nil {
		return nil, err
	}

	var commits []CommitChange
	var cur *CommitChange
	var file *FileDelta
	flushFile := func() {
		if cur != nil && file != nil && (len(file.Added) > 0 || len(file.Removed) > 0) {
			cur.Files = append(cur.Files, *file)
		}
		file = nil
	}
	flushCommit := func() {
		flushFile()
		if cur != nil && len(cur.Files) > 0 {
			commits = append(commits, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "\x00"):
			flushCommit()
			parts := strings.SplitN(line[1:], "\x1f", 3)
			if len(parts) == 3 {
				cur = &CommitChange{SHA: parts[0], Date: parts[1], Subject: parts[2]}
			}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "diff --git"):
			flushFile()
		case strings.HasPrefix(line, "--- a/"):

			if file == nil {
				file = &FileDelta{Path: strings.TrimPrefix(line, "--- a/")}
			}
		case strings.HasPrefix(line, "+++ b/"):

			if file == nil {
				file = &FileDelta{Path: strings.TrimPrefix(line, "+++ b/")}
			} else {
				file.Path = strings.TrimPrefix(line, "+++ b/")
			}
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):

		case file != nil && strings.HasPrefix(line, "+"):
			file.Added = append(file.Added, line[1:])
		case file != nil && strings.HasPrefix(line, "-"):
			file.Removed = append(file.Removed, line[1:])
		}
	}
	flushCommit()
	return commits, nil
}

const (
	ChangesFileName = "changes.md"

	ConsumeMarker = "CONSUMED"
)

func RenderChanges(consumer, from, through string, changes []CommitChange) string {
	var b strings.Builder
	b.WriteString("# Pending knowledge changes\n\n")
	fmt.Fprintf(&b, "- Routine: %s\n", consumer)
	if from == "" {
		b.WriteString("- From: (first run -- cursor starts at the current state; history before it is not replayed)\n")
	} else {
		fmt.Fprintf(&b, "- From: %.12s\n", from)
	}
	fmt.Fprintf(&b, "- Through: %.12s\n", through)
	if len(changes) == 0 {
		b.WriteString("\nNo pending changes.\n")
		return b.String()
	}
	for _, c := range changes {
		fmt.Fprintf(&b, "\n## %s %s (%.12s)\n", c.Date, c.Subject, c.SHA)
		for _, f := range c.Files {
			fmt.Fprintf(&b, "\n### %s\n\n", f.Path)
			for _, l := range f.Added {
				fmt.Fprintf(&b, "+ %s\n", l)
			}
			for _, l := range f.Removed {
				fmt.Fprintf(&b, "- %s\n", l)
			}
		}
	}
	return b.String()
}
