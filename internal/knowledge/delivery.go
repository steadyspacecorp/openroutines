package knowledge

// Delivery: the knowledge branch is a change feed. Every knowledge-writing run
// already leaves a commit; a reporting routine (frontmatter `reports: true`)
// reads the commits since its own cursor as an injected changes.md, and the
// cursor advances only when the routine explicitly consumes the whole change
// set. See design decision "Delivery: the knowledge branch is the change feed".

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/repository"
)

func gitExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// Records how far one consumer has processed knowledge history. Cursors
// are supervisor-owned bookkeeping (under state/, never staged into a run)
// and travel with the knowledge branch like the rest of scheduling state.
type Cursor struct {
	ConsumedThrough string    `json:"consumed_through"` // knowledge commit SHA
	ByRun           string    `json:"by_run"`
	At              time.Time `json:"at"`
}

// The cursor's branch-relative path -- what diagnostics name,
// because repair means editing this file on the knowledge branch.
func CursorFile(consumer string) string {
	return path.Join(stateDirName, "cursors", consumer+".json")
}

func (store *Store) cursorPath(consumer string) string {
	return filepath.Join(store.Worktree(), CursorFile(consumer))
}

// Returns nil when the consumer has no cursor yet (first run).
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

// Writes the cursor into the worktree; the caller's next Commit
// carries it, so consumption is recorded in the run's completion commit.
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

// Lists every consumer with a cursor, for `openroutines status`.
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

// Returns the knowledge branch's current commit: the fixed `through`
// boundary a change set is prepared against.
func (store *Store) Head() (string, error) {
	return repository.Run(store.Worktree(), "rev-parse", "HEAD")
}

// One knowledge commit's model-facing changes.
type CommitChange struct {
	SHA     string
	Date    string // committer date, YYYY-MM-DD
	Subject string
	Files   []FileDelta
}

// One file's added and removed lines within a commit.
type FileDelta struct {
	Path    string
	Added   []string
	Removed []string
}

// Paths a consumer never sees: framework bookkeeping
// (scheduling state, cursors, run records) and routine-private ledgers.
var deliveryExcludes = []string{":(exclude)state", ":(exclude)runs.jsonl", ":(exclude)ledgers"}

// Reports a cursor that no longer names a commit on the
// knowledge branch -- a repaired history, a hand-edited cursor file. The range
// cannot be walked until a person fixes the cursor, so callers should treat
// it as a failure to diagnose rather than one to retry.
var ErrCursorUnreachable = errors.New("consumer cursor is not on the knowledge branch")

// Checks that the cursor commit exists and the boundary descends
// from it -- a commit left behind by a repaired history is still in the
// object store, and walking from it would deliver a change set nobody made.
// Only git's own "no" (exit 1) counts; anything else is git failing to
// answer, which must not abandon a run.
func (store *Store) reachable(from, through string) error {
	wt := store.Worktree()
	full, err := repository.Run(wt, "rev-parse", "--verify", "--quiet", from+"^{commit}")
	if err != nil {
		if gitExitCode(err) != 1 {
			return fmt.Errorf("delivery changes: reading cursor commit: %w", err)
		}
		return fmt.Errorf("%w: commit %.12s is not in this repository", ErrCursorUnreachable, from)
	}
	// Exit 1 here means no common ancestor at all -- as off-branch as a
	// cursor gets, and reported as such by the empty base.
	base, err := repository.Run(wt, "merge-base", full, through)
	if err != nil && gitExitCode(err) != 1 {
		return fmt.Errorf("delivery changes: relating cursor to boundary: %w", err)
	}
	if base != full {
		return fmt.Errorf("%w: commit %.12s is not an ancestor of %.12s", ErrCursorUnreachable, from, through)
	}
	return nil
}

// Walks every commit in (from, through], returning per-commit line
// additions and removals. Commit-by-commit, never a net endpoint diff: an
// event added and later pruned by retention must still reach a consumer that
// hasn't seen it.
func (store *Store) Changes(from, through string) ([]CommitChange, error) {
	if from == "" || through == "" {
		return nil, fmt.Errorf("delivery changes: empty commit range")
	}
	if err := store.reachable(from, through); err != nil {
		return nil, err
	}
	// The %x00/%x1f sentinels are emitted by git itself -- argv cannot carry
	// a literal NUL. Retention trims are dropped: delivering their removals
	// would re-present consumed history to every consumer daily.
	args := append([]string{
		"log", "--reverse", "--date=format:%Y-%m-%d",
		"--format=%x00%H%x1f%ad%x1f%s", "-p", "-U0", "--no-color",
		"--invert-grep", "--grep=^" + trimTrailer + "$",
		from + ".." + through, "--", ".",
	}, deliveryExcludes...)
	out, err := repository.Run(store.Worktree(), args...)
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

const (
	// Injected as a generated snapshot into a reporting routine's workspace.
	ChangesFileName = "changes.md"
	// Created by the routine to consume the whole change set.
	ConsumeMarker = "CONSUMED"
)

// Formats the pending change set as the markdown a reporting
// run reads. Pure data: consume instructions live in the generated definition.
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
