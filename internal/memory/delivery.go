package memory

// Delivery: the memory branch is a change feed. Every memory-writing run
// already leaves a commit; a consumer routine (frontmatter `consumes: memory`)
// reads the commits since its own cursor as an injected inbox, and the cursor
// advances only when the routine explicitly consumes the whole change set.
// See design decision "Delivery: the memory branch is the change feed".

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Cursor records how far one consumer has processed memory history. Cursors
// are supervisor-owned bookkeeping (under state/, never staged into a run)
// and travel with the memory branch like the rest of scheduling state.
type Cursor struct {
	ConsumedThrough string    `json:"consumed_through"` // memory commit SHA
	ByRun           string    `json:"by_run"`
	At              time.Time `json:"at"`
}

func (m *Memory) cursorPath(consumer string) string {
	return filepath.Join(m.StateDir(), "cursors", consumer+".json")
}

// LoadCursor returns nil when the consumer has no cursor yet (first run).
func (m *Memory) LoadCursor(consumer string) (*Cursor, error) {
	raw, err := os.ReadFile(m.cursorPath(consumer))
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
	// files live on the memory branch -- an untrusted, human-writable
	// channel. Only a commit SHA is acceptable.
	if !shaPattern.MatchString(c.ConsumedThrough) {
		return nil, fmt.Errorf("cursor %s: consumed_through %q is not a commit SHA", consumer, c.ConsumedThrough)
	}
	return &c, nil
}

// shaPattern matches an abbreviated or full hex commit SHA.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// SaveCursor writes the cursor into the worktree; the caller's next Commit
// carries it, so consumption is recorded in the run's completion commit.
func (m *Memory) SaveCursor(consumer string, c Cursor) error {
	p := m.cursorPath(consumer)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}

// Cursors lists every consumer with a cursor, for `openroutines status`.
func (m *Memory) Cursors() (map[string]Cursor, error) {
	dir := filepath.Join(m.StateDir(), "cursors")
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
		if c, err := m.LoadCursor(name); err == nil && c != nil {
			out[name] = *c
		}
	}
	return out, nil
}

// Head returns the memory branch's current commit: the fixed `through`
// boundary an inbox is prepared against.
func (m *Memory) Head() (string, error) {
	return git(m.Worktree(), "rev-parse", "HEAD")
}

// CommitChange is one memory commit's model-facing changes.
type CommitChange struct {
	SHA     string
	Date    string // committer date, YYYY-MM-DD
	Subject string
	Files   []FileDelta
}

// FileDelta is one file's added and removed lines within a commit.
type FileDelta struct {
	Path    string
	Added   []string
	Removed []string
}

// deliveryExcludes are paths a consumer never sees: framework bookkeeping
// (scheduling state, cursors, run records) and routine-private ledgers.
var deliveryExcludes = []string{":(exclude)state", ":(exclude)runs.jsonl", ":(exclude)ledgers"}

// ErrCursorUnreachable reports a cursor that no longer names a commit on the
// memory branch -- a repaired history, a hand-edited cursor file. The range
// cannot be walked until a person fixes the cursor, so callers should treat
// it as a failure to diagnose rather than one to retry.
var ErrCursorUnreachable = errors.New("consumer cursor is not on the memory branch")

// reachable checks the precondition for from..through naming a change set:
// the cursor commit exists and the boundary descends from it. Both halves
// matter -- a commit left behind by a repaired history is still in the object
// store, and walking from it would deliver a change set nobody made.
func (m *Memory) reachable(from, through string) error {
	wt := m.Worktree()
	full, err := git(wt, "rev-parse", "--verify", "--quiet", from+"^{commit}")
	if err != nil {
		return fmt.Errorf("%w: commit %.12s is not in this repository", ErrCursorUnreachable, from)
	}
	if base, err := git(wt, "merge-base", full, through); err != nil || base != full {
		return fmt.Errorf("%w: commit %.12s is not an ancestor of %.12s", ErrCursorUnreachable, from, through)
	}
	return nil
}

// Changes walks every commit in (from, through], returning per-commit line
// additions and removals. Commit-by-commit, never a net endpoint diff: an
// event added and later pruned by retention must still reach a consumer that
// hasn't seen it.
func (m *Memory) Changes(from, through string) ([]CommitChange, error) {
	if from == "" || through == "" {
		return nil, fmt.Errorf("delivery changes: empty commit range")
	}
	if err := m.reachable(from, through); err != nil {
		return nil, err
	}
	// The commit sentinel and field separators are emitted by git itself
	// (%x00/%x1f) -- argv cannot carry a literal NUL. Retention trims are
	// dropped from the walk: pruning is bookkeeping, not a memory change, and
	// delivering its removals would re-present already-consumed history to
	// every consumer once a day. The pattern is anchored because --grep sees
	// the whole message, subject included, and a routine name is a filename.
	args := append([]string{
		"log", "--reverse", "--date=format:%Y-%m-%d",
		"--format=%x00%H%x1f%ad%x1f%s", "-p", "-U0", "--no-color",
		"--invert-grep", "--grep=^" + trimTrailer + "$",
		from + ".." + through, "--", ".",
	}, deliveryExcludes...)
	out, err := git(m.Worktree(), args...)
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
			// old path: names deleted files, which have no "+++ b/" side
			if file == nil {
				file = &FileDelta{Path: strings.TrimPrefix(line, "--- a/")}
			}
		case strings.HasPrefix(line, "+++ b/"):
			// new path wins when both sides exist (renames, edits, new files)
			if file == nil {
				file = &FileDelta{Path: strings.TrimPrefix(line, "+++ b/")}
			} else {
				file.Path = strings.TrimPrefix(line, "+++ b/")
			}
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):
			// remaining structural lines: /dev/null sides, hunk headers
		case file != nil && strings.HasPrefix(line, "+"):
			file.Added = append(file.Added, line[1:])
		case file != nil && strings.HasPrefix(line, "-"):
			file.Removed = append(file.Removed, line[1:])
		}
	}
	flushCommit()
	return commits, nil
}

// InboxFileName is the read-only delivery inbox injected into a consumer
// routine's workspace; ConsumeMarker is the file the routine creates to
// consume the whole change set.
const (
	InboxFileName = "inbox.md"
	ConsumeMarker = "CONSUMED"
)

// RenderInbox formats the pending change set as the markdown a consumer run
// reads. Pure data: the consume instructions live in the generated agent
// definition, not here.
func RenderInbox(consumer, from, through string, changes []CommitChange) string {
	var b strings.Builder
	b.WriteString("# Pending memory changes\n\n")
	fmt.Fprintf(&b, "- Consumer: %s\n", consumer)
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
