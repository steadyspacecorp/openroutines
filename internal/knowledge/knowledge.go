// Package knowledge manages the agent's knowledge: a git worktree of the orphan
// `knowledge` branch, plus the staging pipeline that keeps model-directed
// processes away from git entirely (see design decision "Knowledge syncs per run").
package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Knowledge is a dedicated directory backed by its own branch.
const (
	Dir        = "knowledge"
	Branch     = "knowledge"
	maxFile    = 5 << 20 // per-file size cap in staged knowledge
	maxEntries = 2000    // total staged file cap
)

// stateDirName is the supervisor-owned directory inside the worktree holding
// per-routine bookkeeping: scheduling state at its root, trigger baselines
// and consumer cursors in subdirectories.
const stateDirName = "state"

// Knowledge is one agent repository's knowledge. The handle binds the repository
// root; every operation that reads or maintains the knowledge branch, its
// worktree, and the supervisor-owned state inside it goes through here.
type Knowledge struct {
	repoDir string
}

// At binds the agent repository at repoDir. No I/O: the worktree may not be
// materialized yet (Status reports that; Ensure fixes it).
func At(repoDir string) *Knowledge { return &Knowledge{repoDir: repoDir} }

// primitives are the framework-blessed shared knowledge files, seeded on init.
// Each opens with a fenced example of its format: the file teaches its own
// shape at the point of use, and the retention trimmer preserves everything
// that isn't a record. Agents may reshape the headers in their own knowledge
// branch -- seeding is create-if-missing and never overwrites.
var primitives = map[string]string{
	"events.md": "# Events\n\nWhat happened: append-only outcomes and observations, full facts, no polish.\n" +
		"Includes explicit NO-OPs -- a run that found nothing to change still happened.\n\n" +
		"Format (one line per entry):\n\n```markdown\n" +
		"- YYYY-MM-DD <routine>: <what happened, why it matters, links, people>\n" +
		"- YYYY-MM-DD <routine> NO-OP: <what was checked and found clean>\n```\n",
	"tasks.md": "# Tasks\n\nWhat must happen, and who owns the next action. One canonical entry per\n" +
		"task, from discovery to resolution: update it in place (complete, cancel,\n" +
		"or move between sections) rather than re-recording it elsewhere. A blocked\n" +
		"task names what it is waiting on.\n\n" +
		"Format:\n\n```markdown\n" +
		"## Agent-owned\n\n" +
		"- [ ] `task-YYYYMMDD-<n>` <description> (routine: <handler>; source: <where it came from>; added YYYY-MM-DD)\n" +
		"- [x] `task-YYYYMMDD-<n>` <description> (source: ...; added YYYY-MM-DD; done YYYY-MM-DD)\n\n" +
		"## Human-owned\n\n" +
		"- [ ] `task-YYYYMMDD-<n>` <ask> (source: <where it came from>; added YYYY-MM-DD)\n```\n",
	"context.md": "# Context\n\nShared situational awareness: facts that may inform future decisions but\n" +
		"require no action. Refresh a fact when a run reaffirms it; stale entries\n" +
		"age out with retention.\n\n" +
		"Format (one line per entry):\n\n```markdown\n" +
		"- YYYY-MM-DD <routine>: <the fact, and where it came from>\n```\n",
}

// supervisorOwned paths never travel into staging and are never touched by
// import: routines cannot read or rewrite scheduling state or run records.
var supervisorOwned = map[string]bool{
	stateDirName: true,
	"runs.jsonl": true,
}

// gitPassthrough is everything a git child inherits from this process --
// what git and ssh need to work at all.
var gitPassthrough = []string{
	"PATH",          // git's own subcommands, ssh, credential helpers
	"HOME",          // ~/.ssh: known_hosts and the operator's keys and config
	"SSH_AUTH_SOCK", // the operator's ssh-agent
	"TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// A TLS-inspecting proxy's CA bundle; the global config that could carry
	// http.sslCAInfo is sent to /dev/null.
	"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO",
}

// gitCmd is a git invocation together with the deadline that bounds it.
type gitCmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
}

// newGitCmd builds a git invocation with a constructed environment and no
// system or global config. Inheriting the environment would publish
// OPENROUTINES_MASTER_KEY in the child's /proc/<pid>/environ -- non-dumpable
// does not survive execve.
func newGitCmd(dir string, args []string) *gitCmd {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(originRewrite), args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	for _, name := range gitPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	if sshCommand != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand)
	}
	// git does the network through children (ssh, git-remote-https); the
	// deadline has to reach the whole group or a stalled transport keeps
	// holding the output pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGitGroup(cmd.Process.Pid) }
	cmd.WaitDelay = gitDrainDeadline
	return &gitCmd{Cmd: cmd, ctx: ctx, cancel: cancel}
}

// killGitGroup ends a timed-out invocation's process group: SIGTERM, grace,
// SIGKILL. The grace matters: git releases its lock files on SIGTERM but not
// SIGKILL, and a stranded lock fails every later invocation.
func killGitGroup(pid int) error {
	pgid := -pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	time.Sleep(gitKillGrace)
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// fail wraps a failed invocation, naming the deadline when it was the cause
// ("signal: killed" on a fetch would read as a supervisor bug, not an origin
// gone dark). An abandoned drain is a failure even though git exited cleanly:
// the output is truncated and callers parse it. Only the last case keeps the
// error in the chain -- gitExitCode must never read an exit status out of a
// deadline we imposed.
func (c *gitCmd) fail(args []string, err error, out []byte) error {
	switch {
	case c.ctx.Err() != nil:
		return fmt.Errorf("git %s: timed out after %s: %s", strings.Join(args, " "), gitTimeout, strings.TrimSpace(string(out)))
	case errors.Is(err, exec.ErrWaitDelay):
		return fmt.Errorf("git %s: exited cleanly but something it spawned still held the output pipe after %s, so the output is incomplete: %s", strings.Join(args, " "), gitDrainDeadline, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

// The bounds on a git invocation; the drain has to outlast the grace.
// gitTimeout sits above the transport's low-speed bounds because those only
// fire once bytes move -- a blackholed connect parks for many minutes.
// Variables so tests can drive them.
var (
	gitTimeout       = 2 * time.Minute
	gitKillGrace     = 2 * time.Second
	gitDrainDeadline = 5 * time.Second
)

// hermeticConfig is the -c configuration every git invocation carries: no
// hooks, a fixed commit identity, no auto-gc (a detached gc keeps writing
// .git/objects after the command returns), and low-speed bounds so a quiet
// transfer is abandoned rather than parked forever.
var hermeticConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.file.allow=user",
	// A repo-config grep.patternType=fixed would break the delivery feed's
	// anchored --grep and quietly put every retention trim back in it.
	"-c", "grep.patternType=basic",
	"-c", "user.name=openroutines",
	"-c", "user.email=agent@openroutines.dev",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "http.lowSpeedLimit=1000",
	"-c", "http.lowSpeedTime=60",
}

// git runs a git command against the repo with hermetic configuration:
// no system/global config leaks in.
func git(dir string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitExitCode reports the status git exited with, or -1 when it never got to
// exit -- "git answered no" versus "git could not answer".
func gitExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// Worktree returns the knowledge worktree location inside the agent repo.
func (m *Knowledge) Worktree() string { return filepath.Join(m.repoDir, Dir) }

// StateDir returns the supervisor-owned state directory inside the worktree.
// Per-routine state lives at <StateDir>/<name>.json (scheduling) and
// <StateDir>/<subdir>/<name>.json (trigger baselines, consumer cursors).
func (m *Knowledge) StateDir() string { return filepath.Join(m.Worktree(), stateDirName) }

// Ensure materializes knowledge/ as a worktree of the knowledge branch,
// creating the orphan branch and seeding the primitives on first use.
// Self-heals: safe to call every run.
func (m *Knowledge) Ensure() error {
	wt := m.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return nil // already materialized
	}
	// The image's .git may register a worktree whose directory was excluded
	// from the image; prune or the add below fails on first boot.
	_, _ = git(m.repoDir, "worktree", "prune")
	if _, err := git(m.repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// No local branch: adopt origin's rather than minting a new root
		// that splices into the lineage.
		if m.HasOrigin() {
			if _, lerr := git(m.repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); lerr == nil {
				if _, ferr := git(m.repoDir, "fetch", "--quiet", "origin", "+refs/heads/"+Branch+":refs/heads/"+Branch); ferr != nil {
					return fmt.Errorf("adopting knowledge branch from origin: %w", ferr)
				}
				// Adoption is where a restart could launder a rewritten
				// history: refuse a tip that does not descend from the
				// accepted baseline, which survives container replacement.
				if accepted := m.AcceptedTip(); accepted != "" {
					tip, terr := git(m.repoDir, "rev-parse", "refs/heads/"+Branch)
					if terr == nil && tip != accepted && !isAncestor(m.repoDir, accepted, tip) {
						return fmt.Errorf("origin/%s does not descend from the last accepted tip %s -- knowledge history was rewritten while this agent was down; refusing to adopt it. Inspect origin/%s, then either restore the branch or move %s to the new tip to accept the rewrite deliberately", Branch, short(accepted), Branch, acceptedRef)
					}
				}
				tip, _ := git(m.repoDir, "rev-parse", "refs/heads/"+Branch)
				slog.Info("knowledge: adopted the knowledge branch from origin", "tip", tip)
			} else if !strings.Contains(lerr.Error(), "exit status 2") {
				slog.Warn("knowledge: could not reach origin to adopt the knowledge branch -- creating a local root; this will diverge if origin has one", "error", lerr)
			}
		}
	}
	if _, err := git(m.repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+Branch); err != nil {
		// First use: orphan branch from an empty tree via plumbing
		// (worktree add --orphan needs git >= 2.42).
		tree, err := gitStdin(m.repoDir, "", "mktree")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		commit, err := git(m.repoDir, "commit-tree", tree, "-m", "Knowledge branch root")
		if err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		if _, err := git(m.repoDir, "branch", Branch, commit); err != nil {
			return fmt.Errorf("creating knowledge branch: %w", err)
		}
		slog.Info("knowledge: created the knowledge branch", "commit", commit)
	}
	if _, err := git(m.repoDir, "worktree", "add", wt, Branch); err != nil {
		return err
	}
	// Seed the primitives and the ledgers directory if absent.
	for name, content := range primitives {
		p := filepath.Join(wt, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(wt, "ledgers"), 0o755); err != nil {
		return err
	}
	if _, err := git(wt, "add", "-A"); err != nil {
		return err
	}
	if changed, _ := git(wt, "status", "--porcelain"); changed != "" {
		if _, err := git(wt, "commit", "--quiet", "-m", "Seed knowledge primitives"); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot copies the knowledge worktree's files into a plain staging directory:
// regular files only, no git metadata. This staged copy is what a routine
// sees and writes as knowledge/.
func (m *Knowledge) Snapshot(stagingDir string) error {
	wt := m.Worktree()
	return filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil || rel == "." {
			return err
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(stagingDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks etc. never travel into staging
		}
		return copyFile(path, dest)
	})
}

func topSegment(rel string) string {
	return strings.Split(rel, string(filepath.Separator))[0]
}

// CloneTree copies a snapshot tree verbatim, so the run's staged copy and the
// import's base come from one worktree read.
func CloneTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		dest := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, dest)
	})
}

// stagedPathPolicy rejects paths that may never enter the worktree: git
// control files, supervisor-owned bookkeeping, absurd depth. Applied by
// Validate and again by the import copy -- the tree can change under the walk
// that validated it.
func stagedPathPolicy(rel string, isDir bool) error {
	switch filepath.Base(rel) {
	case ".git", ".gitattributes", ".gitmodules", ".gitignore":
		return fmt.Errorf("staged knowledge contains git control file %q -- rejected", rel)
	}
	if supervisorOwned[topSegment(rel)] {
		return fmt.Errorf("staged knowledge touches supervisor-owned path %q -- rejected", rel)
	}
	if isDir && strings.Count(rel, string(filepath.Separator)) > 8 {
		return fmt.Errorf("staged knowledge path %q too deep -- rejected", rel)
	}
	return nil
}

// Validate rejects a staged knowledge tree that contains anything but regular
// files under sane limits. A rejected tree fails the whole run.
func Validate(stagingDir string) error {
	entries := 0
	return filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagingDir, path)
		if err != nil || rel == "." {
			return err
		}
		if err := stagedPathPolicy(rel, d.IsDir()); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("staged knowledge contains non-regular file %q -- rejected", rel)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("staged knowledge exceeds %d files -- rejected", maxEntries)
		}
		if info, err := d.Info(); err == nil {
			if info.Size() > maxFile {
				return fmt.Errorf("staged knowledge file %q exceeds %d bytes -- rejected", rel, maxFile)
			}
			// A hard link can alias a file outside the staging tree.
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return fmt.Errorf("staged knowledge file %q is a hard link -- rejected", rel)
			}
		}
		return nil
	})
}

// Import applies the staged tree to the worktree as a three-way merge against
// the base snapshot: an untouched file imports nothing (a stale copy must
// never regress concurrent settlements), a file only the run changed copies
// whole, appends on both sides compose, and any other concurrent edit keeps
// the canonical file and quarantines the staged competitor. Deletions apply
// only where the worktree still matches the base. Caller commits.
func (m *Knowledge) Import(stagingDir, baseDir string) (conflicted []Conflict, err error) {
	if err := Validate(stagingDir); err != nil {
		return nil, err
	}
	wt := m.Worktree()
	// Refuse to import over uncommitted human curation -- it has no reflog to
	// recover from. Supervisor-owned paths legitimately carry this attempt's
	// own in-flight bookkeeping.
	if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			// git() trims the output, eating the first line's status column;
			// a path containing spaces degrades toward refusal, never toward
			// a silent import.
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			path := fields[len(fields)-1]
			if !supervisorOwned[topSegment(path)] {
				return nil, fmt.Errorf("knowledge worktree has uncommitted changes (%s) -- refusing to import over them; commit or discard (git -C %s ...) and re-run", path, Dir)
			}
		}
	}
	conflicted, err = copyStaged(stagingDir, baseDir, wt)
	if err != nil {
		return nil, err
	}
	// Apply staged deletions, but only where the worktree still matches the
	// base -- a file another run created or changed is theirs to keep.
	return conflicted, filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(wt, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(stagingDir, rel)); !os.IsNotExist(err) {
			return nil
		}
		base, berr := os.ReadFile(filepath.Join(baseDir, rel))
		if berr != nil {
			return nil // the run never saw this file; it cannot delete it
		}
		cur, cerr := os.ReadFile(path)
		if cerr == nil && bytes.Equal(cur, base) {
			return os.Remove(path)
		}
		if cerr == nil {
			slog.Debug("knowledge: kept a file the run deleted -- the worktree moved since its snapshot", "path", rel)
		}
		return nil
	})
}

// RestoreFile puts the base-snapshot copy of one knowledge file back into the
// staged tree -- restored to base, not the live worktree, so the import's
// unchanged-versus-base rule then skips it. The enforcement half of
// `teamwork: off`. Reports whether a staged change was discarded.
func RestoreFile(stagingDir, baseDir, name string) (bool, error) {
	want, werr := os.ReadFile(filepath.Join(baseDir, name))
	if werr != nil && !os.IsNotExist(werr) {
		return false, werr
	}
	// Confined like the import copy: a path swapped for a symlink must not
	// redirect the write out of the staging tree.
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	staged, serr := openStaged(root, name)
	if serr == nil {
		defer func() { _ = staged.Close() }()
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return false, serr
	}
	if os.IsNotExist(werr) {
		// The snapshot had no such file: the run must not create it either.
		if serr != nil {
			return false, nil
		}
		return true, root.Remove(name)
	}
	if serr == nil {
		got, err := io.ReadAll(staged)
		if err != nil {
			return false, err
		}
		if bytes.Equal(got, want) {
			return false, nil
		}
	}
	return true, root.WriteFile(name, want, 0o644)
}

// Commit records the current worktree state on the knowledge branch.
func (m *Knowledge) Commit(message string) (string, error) {
	wt := m.Worktree()
	if _, err := git(wt, "add", "-A"); err != nil {
		return "", err
	}
	if changed, _ := git(wt, "status", "--porcelain"); changed == "" {
		return "", nil // nothing to commit
	}
	if _, err := git(wt, "commit", "--quiet", "-m", message); err != nil {
		return "", err
	}
	return git(wt, "rev-parse", "--short", "HEAD")
}

// commitPaths commits only the named paths: a maintenance commit must not
// carry work that merely happened to be dirty when it fired. Missing paths
// are skipped.
func (m *Knowledge) commitPaths(message string, paths ...string) (string, error) {
	wt := m.Worktree()
	var present []string
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(wt, p)); err == nil {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	// A pathspec commit only covers tracked paths -- add first so a new file
	// isn't a pathspec that matches nothing.
	if _, err := git(wt, append([]string{"add", "--"}, present...)...); err != nil {
		return "", err
	}
	if changed, _ := git(wt, append([]string{"status", "--porcelain", "--"}, present...)...); changed == "" {
		return "", nil // nothing to commit
	}
	if _, err := git(wt, append([]string{"commit", "--quiet", "-m", message, "--"}, present...)...); err != nil {
		return "", err
	}
	return git(wt, "rev-parse", "--short", "HEAD")
}

// flatten collapses whitespace runs to single spaces, honoring the
// one-line-per-entry format of events.md and tasks.md.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// scrubbed prepares supervisor-written text for a knowledge file: redacted at
// this seam, not at call sites that remember to ask -- what lands here is
// committed and pushed.
func (m *Knowledge) scrubbed(line string) string {
	return flatten(scrub.Redacted(line))
}

// AppendEvent records a supervisor-written event: the mechanism for outcomes
// the model never got to narrate (timeouts, crashes, sync trouble).
func (m *Knowledge) AppendEvent(line string) error {
	p := filepath.Join(m.Worktree(), "events.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", m.scrubbed(line))
	return err
}

// AppendHumanTask records a supervisor-created human-owned task at the end of
// the real "## Human-owned" section (fenced format examples don't count),
// created if missing. Idempotent by task id.
func (m *Knowledge) AppendHumanTask(taskID, description string) error {
	p := filepath.Join(m.Worktree(), "tasks.md")
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if text == "" {
		text = "# Tasks\n"
	}
	if strings.Contains(text, "`"+taskID+"`") {
		return nil // one canonical record per task
	}
	entry := fmt.Sprintf("- [ ] `%s` %s", taskID, m.scrubbed(description))
	lines := strings.Split(text, "\n")

	section := -1
	inFence := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && t == "## Human-owned" {
			section = i
		}
	}
	if section < 0 {
		text = strings.TrimRight(text, "\n") + "\n\n## Human-owned\n\n" + entry + "\n"
		return os.WriteFile(p, []byte(text), 0o644)
	}
	// End of the section: the next unfenced heading, else EOF; insert before
	// the section's trailing blank lines.
	end := len(lines)
	inFence = false
	for i := section + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(t, "## ") {
			end = i
			break
		}
	}
	for end > section+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := slices.Insert(lines, end, entry)
	return os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644)
}

// ResolveHumanTasks completes every open task whose id starts with idPrefix.
// Prefix matching makes recovery restart-proof: the supervisor need not
// remember which day's blocker it raised. Reports whether anything changed.
func (m *Knowledge) ResolveHumanTasks(idPrefix, resolution string) (bool, error) {
	p := filepath.Join(m.Worktree(), "tasks.md")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "- [ ] `"+idPrefix) {
			continue
		}
		line = strings.Replace(line, "- [ ]", "- [x]", 1)
		if trimmed := strings.TrimRight(line, " "); strings.HasSuffix(trimmed, ")") {
			at := strings.LastIndex(line, ")")
			line = line[:at] + "; " + resolution + ")"
		} else {
			line += " (" + resolution + ")"
		}
		lines[i] = line
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
}

// AppendRunRecord appends one JSONL run record. Redacted like every append
// but never flattened: whitespace inside its JSON strings is content.
func (m *Knowledge) AppendRunRecord(record string) error {
	p := filepath.Join(m.Worktree(), "runs.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, scrub.Redacted(record))
	return err
}

// RemoveRoutineState deletes every per-routine state file for name: the
// scheduling state plus the entry in every state subdirectory. Filenames are
// compared, never globbed, so name cannot alter the matching. Returns the
// removed paths relative to the repository root; the caller commits.
func (m *Knowledge) RemoveRoutineState(name string) ([]string, error) {
	stateDir := m.StateDir()
	entries, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	remove := func(dir string) error {
		p := filepath.Join(dir, name+".json")
		switch err := os.Remove(p); {
		case err == nil:
			rel, _ := filepath.Rel(m.repoDir, p)
			removed = append(removed, rel)
		case !os.IsNotExist(err):
			return err
		}
		return nil
	}
	if err := remove(stateDir); err != nil {
		return removed, err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := remove(filepath.Join(stateDir, e.Name())); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// WorktreeStatus reports the knowledge worktree's state for `openroutines
// status` -- root `git status` never shows knowledge churn, so this must.
type WorktreeStatus struct {
	Materialized    bool
	RemoteKnowledge bool   // origin/knowledge ref exists locally: the agent has history even if this checkout hasn't adopted it
	Uncommitted     int    // files with uncommitted changes (human curation in progress)
	LastCommit      string // subject of the latest knowledge commit
	Unpushed        int    // commits origin hasn't seen yet
	Behind          int    // commits on origin this worktree has not taken
}

// Status inspects the knowledge worktree; only RemoteKnowledge is set when not
// yet materialized -- it distinguishes a fresh clone of a running agent
// (adopt with sync) from an agent that has never run.
func (m *Knowledge) Status() WorktreeStatus {
	var st WorktreeStatus
	if _, err := git(m.repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch); err == nil {
		st.RemoteKnowledge = true
	}
	wt := m.Worktree()
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		return st
	}
	st.Materialized = true
	if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
		st.Uncommitted = len(strings.Split(out, "\n"))
	}
	if out, err := git(wt, "log", "-1", "--format=%h %s"); err == nil {
		st.LastCommit = out
	}
	if out, err := git(wt, "rev-list", "--count", "refs/remotes/origin/"+Branch+"..HEAD"); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Unpushed)
	}
	// As of the last fetch: the remote-tracking ref is local, so this stays
	// offline.
	if out, err := git(wt, "rev-list", "--count", "HEAD..refs/remotes/origin/"+Branch); err == nil {
		_, _ = fmt.Sscanf(out, "%d", &st.Behind)
	}
	return st
}

// Conflict records a semantic concurrent edit and the durable path where the
// competing staged version was preserved.
type Conflict struct {
	Path       string
	Quarantine string
}

// copyStaged brings every staged file into the worktree. Staging is not
// quiescent -- a descendant of the model process can outlive the run and
// rewrite what Validate approved -- so an os.Root confines every path and
// every check is re-applied on the descriptor being read. The copy lands in a
// scratch tree and is promoted only once the whole staged tree has passed:
// a mid-walk rejection must not leave a half-imported worktree for Settle to
// commit.
func copyStaged(stagingDir, baseDir, wt string) (conflicted []Conflict, err error) {
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	scratch, err := os.MkdirTemp(filepath.Dir(wt), ".openroutines-import-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	var dirs, files []string
	if err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.FromSlash(rel)
		if rel == ConsumeMarker {
			return nil // consume receipt for the runtime, never knowledge content
		}
		if err := stagedPathPolicy(rel, d.IsDir()); err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return os.MkdirAll(filepath.Join(scratch, rel), 0o755)
		}
		if len(files) >= maxEntries {
			return fmt.Errorf("staged knowledge exceeds %d files -- rejected", maxEntries)
		}
		files = append(files, rel)
		return copyStagedFile(root, rel, filepath.Join(scratch, rel))
	}); err != nil {
		return nil, err
	}
	for _, rel := range dirs {
		if err := os.MkdirAll(filepath.Join(wt, rel), 0o755); err != nil {
			return nil, err
		}
	}
	// The three-way decision, on trusted bytes: scratch copy, base, and
	// worktree are all supervisor-owned by now. Every file's final bytes
	// resolve into the scratch tree first; the rename-only pass below is
	// what keeps promotion all-or-nothing.
	var promote []string
	var quarantines []string
	for _, rel := range files {
		staged, err := os.ReadFile(filepath.Join(scratch, rel))
		if err != nil {
			return nil, err
		}
		base, berr := os.ReadFile(filepath.Join(baseDir, rel))
		if berr != nil && !os.IsNotExist(berr) {
			return nil, berr
		}
		if berr == nil && bytes.Equal(staged, base) {
			continue // untouched by the run: never regress what settled since
		}
		cur, cerr := os.ReadFile(filepath.Join(wt, rel))
		if cerr != nil && !os.IsNotExist(cerr) {
			return nil, cerr
		}
		if !os.IsNotExist(cerr) && !bytes.Equal(cur, base) && !bytes.Equal(cur, staged) {
			// A concurrently settled run changed the same file.
			if merged, ok := appendMerge(cur, base, staged); ok {
				if err := os.WriteFile(filepath.Join(scratch, rel), merged, 0o644); err != nil {
					return nil, err
				}
			} else {
				sum := sha256.Sum256(staged)
				quarantine := filepath.Join("state", "conflicts", fmt.Sprintf("%x", sum[:8]), rel)
				source := filepath.Join(scratch, rel)
				target := filepath.Join(scratch, quarantine)
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return nil, err
				}
				if err := os.Rename(source, target); err != nil {
					return nil, err
				}
				conflicted = append(conflicted, Conflict{Path: rel, Quarantine: quarantine})
				quarantines = append(quarantines, quarantine)
				continue // the last valid canonical file stays untouched
			}
		}
		promote = append(promote, rel)
	}
	promote = append(promote, quarantines...)
	for _, rel := range promote {
		dest := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(scratch, rel), dest); err != nil {
			return nil, err
		}
	}
	return conflicted, nil
}

// appendMerge composes only the shape we can prove safe: both descendants
// retain the complete base and add bytes at its end. Semantic edits belong in
// quarantine, never in an automatic union.
func appendMerge(ours, base, theirs []byte) ([]byte, bool) {
	if !bytes.HasPrefix(ours, base) || !bytes.HasPrefix(theirs, base) {
		return nil, false
	}
	merged := make([]byte, 0, len(ours)+len(theirs)-len(base))
	merged = append(merged, base...)
	merged = append(merged, ours[len(base):]...)
	merged = append(merged, theirs[len(base):]...)
	return merged, true
}

// copyStagedFile copies one staged file into the scratch tree, bounded by the
// same size cap Validate measured against: the file can have grown since.
func copyStagedFile(root *os.Root, rel, dest string) error {
	in, err := openStaged(root, rel)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, maxFile+1))
	if err != nil {
		return err
	}
	if n > maxFile {
		return fmt.Errorf("staged knowledge file %q exceeds %d bytes -- rejected", rel, maxFile)
	}
	return nil
}

// openStaged opens a path inside the staging tree and proves on the
// descriptor itself that it is an ordinary unaliased file -- nothing an
// earlier stat decided is trusted. O_NONBLOCK so a fifo cannot park the
// caller.
func openStaged(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("staged knowledge file %q is not readable inside staging -- rejected: %w", rel, err)
	}
	info, err := f.Stat()
	switch {
	case err != nil:
	case !info.Mode().IsRegular():
		err = fmt.Errorf("staged knowledge file %q is not a regular file -- rejected", rel)
	default:
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			err = fmt.Errorf("staged knowledge file %q is a hard link -- rejected", rel)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
