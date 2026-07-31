package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

func TestValidateAcceptsPlainFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ledgers"), 0o755)
	os.WriteFile(filepath.Join(dir, "events.md"), []byte("fact\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ledgers", "x.md"), []byte("state\n"), 0o644)
	if err := Validate(dir); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateRejectsGitControlFiles(t *testing.T) {
	for _, name := range []string{".gitattributes", ".gitmodules", ".gitignore"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		if err := Validate(dir); err == nil {
			t.Fatalf("expected rejection for %s", name)
		}
	}
}

func TestValidateRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for symlink")
	}
}

func TestValidateRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxFile+1)
	os.WriteFile(filepath.Join(dir, "big.md"), big, 0o644)
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for oversized file")
	}
}

func TestValidateRejectsHardLinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	if err := os.Link(outside, filepath.Join(dir, "alias.md")); err != nil {
		t.Skip("hard links unavailable")
	}
	if err := Validate(dir); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("expected hard-link rejection, got %v", err)
	}
}

// Staging is not quiescent between validation and import: a descendant of
// the model process can outlive it and swap a staged file for a symlink
// after Validate has walked the tree. The copy path is therefore tested
// directly, with no Validate in front of it: what it copies has to be
// decided by the descriptor it reads from, not by an earlier walk.
func TestStagedCopyNeverFollowsSymlinks(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	staging, wt := t.TempDir(), t.TempDir()
	if err := os.Symlink(secret, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
		t.Error("expected the copy to refuse a symlinked staged file")
	}
	if raw, _ := os.ReadFile(filepath.Join(wt, "events.md")); strings.Contains(string(raw), "SECRET") {
		t.Fatalf("the symlink target was copied into the worktree: %q", raw)
	}
}

// Same window, the alias a path check cannot see: a hard link the copy path
// must recognize on the open file, not trust Validate to have caught.
func TestStagedCopyRefusesHardLinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("SECRET"), 0o644)
	staging, wt := t.TempDir(), t.TempDir()
	if err := os.Link(outside, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("hard links unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Errorf("expected hard-link rejection, got %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(wt, "events.md")); strings.Contains(string(raw), "SECRET") {
		t.Fatalf("the hard link's content was copied into the worktree: %q", raw)
	}
}

// The whole policy is re-applied at copy time, not just the file-type half:
// a path Validate walked past and a descendant created afterwards is still a
// path that must never enter the branch.
func TestStagedCopyRefusesPathsValidateWouldReject(t *testing.T) {
	for _, rel := range []string{".gitattributes", ".gitignore", filepath.Join(stateDirName, "sched.md"), "runs.jsonl"} {
		staging, wt := t.TempDir(), t.TempDir()
		os.MkdirAll(filepath.Dir(filepath.Join(staging, rel)), 0o755)
		os.WriteFile(filepath.Join(staging, rel), []byte("x\n"), 0o644)
		if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
			t.Errorf("%s: expected the copy to refuse it", rel)
		}
		if _, err := os.Stat(filepath.Join(wt, rel)); !os.IsNotExist(err) {
			t.Errorf("%s: copied into the worktree anyway", rel)
		}
	}
}

// The size cap too: a file Validate measured can be grown before the copy
// reads it, so the cap is enforced on the bytes actually copied.
func TestStagedCopyRefusesOversizedFile(t *testing.T) {
	staging, wt := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(staging, "events.md"), make([]byte, maxFile+1), 0o644)
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an oversize rejection, got %v", err)
	}
}

// Rejecting halfway through must leave the worktree as it was found. Settle
// commits the failure record, so a half-copied tree would be committed as the
// failed run's memory -- exactly the atomicity staging exists to provide.
func TestStagedCopyRejectionLeavesTheWorktreeUntouched(t *testing.T) {
	staging, wt := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(wt, "events.md"), []byte("committed\n"), 0o644)
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("committed\n- new\n"), 0o644)
	os.WriteFile(filepath.Join(staging, "tasks.md"), []byte("- [ ] new\n"), 0o644)
	// Sorts last: the good files are copied before the rejection lands.
	if err := os.Symlink(filepath.Join(t.TempDir(), "secret.txt"), filepath.Join(staging, "zz.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
		t.Fatal("expected the copy to refuse the symlink")
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "committed\n" {
		t.Errorf("worktree events.md = %q, want the pre-import content", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "tasks.md")); !os.IsNotExist(err) {
		t.Error("a file from the rejected tree landed in the worktree")
	}
}

// RestoreFile writes into staging after the run, so it needs the import
// copy's confinement: a staged path swapped for a symlink must not redirect
// the write out of the staging tree.
func TestRestoreFileNeverWritesOutsideStaging(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "events.md"), []byte("base events\n"), 0o644)
	staging := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("do not touch\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := RestoreFile(staging, base, "events.md"); err == nil {
		t.Error("expected RestoreFile to refuse a symlinked staged path")
	}
	if got, _ := os.ReadFile(outside); string(got) != "do not touch\n" {
		t.Fatalf("wrote through the symlink: %q", got)
	}
}

// Import must refuse to overwrite uncommitted human curation -- there is no
// reflog for edits that were never committed. Supervisor-owned paths are the
// attempt's own in-flight bookkeeping and do not gate.
func TestImportRefusesDirtyWorktree(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main", ".")
	if err := At(repo).Ensure(); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n"), 0o644)

	// A human edit, uncommitted: refuse.
	wt := At(repo).Worktree()
	os.WriteFile(filepath.Join(wt, "tasks.md"), []byte("- [ ] mid-edit\n"), 0o644)
	if _, err := At(repo).Import(staging, t.TempDir()); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("expected dirty-worktree refusal, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "tasks.md")); string(got) != "- [ ] mid-edit\n" {
		t.Fatalf("refused import still modified the worktree: %q", got)
	}

	// Committed: import proceeds.
	if _, err := At(repo).Commit("human curation"); err != nil {
		t.Fatal(err)
	}
	if _, err := At(repo).Import(staging, t.TempDir()); err != nil {
		t.Fatalf("clean worktree should import: %v", err)
	}

	// The pipeline commits right after a successful import; mirror that.
	if _, err := At(repo).Commit("import"); err != nil {
		t.Fatal(err)
	}

	// Supervisor-owned dirt (this attempt's own bookkeeping) does not gate.
	os.MkdirAll(filepath.Join(wt, "state"), 0o755)
	os.WriteFile(filepath.Join(wt, "state", "r.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n- another\n"), 0o644)
	if _, err := At(repo).Import(staging, t.TempDir()); err != nil {
		t.Fatalf("supervisor-owned dirt must not block import: %v", err)
	}
}

func TestRestoreFileDiscardsStagedChange(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "events.md"), []byte("base\n"), 0o644)
	staging := t.TempDir()

	// A staged edit is undone: the base copy wins, so the import's
	// unchanged-versus-base rule then skips the file.
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
	changed, err := RestoreFile(staging, base, "events.md")
	if err != nil || !changed {
		t.Fatalf("edited file: changed=%v err=%v, want true nil", changed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(staging, "events.md")); string(got) != "base\n" {
		t.Fatalf("staged events.md = %q, want base copy restored", got)
	}

	// An untouched file reports no change.
	changed, err = RestoreFile(staging, base, "events.md")
	if err != nil || changed {
		t.Fatalf("untouched file: changed=%v err=%v, want false nil", changed, err)
	}

	// A staged deletion is undone too -- import would otherwise delete it.
	os.Remove(filepath.Join(staging, "events.md"))
	changed, err = RestoreFile(staging, base, "events.md")
	if err != nil || !changed {
		t.Fatalf("deleted file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "events.md")); err != nil {
		t.Fatal("staged events.md not restored after deletion")
	}

	// A file the snapshot never had must not be created through staging.
	os.WriteFile(filepath.Join(staging, "novel.md"), []byte("x\n"), 0o644)
	changed, err = RestoreFile(staging, base, "novel.md")
	if err != nil || !changed {
		t.Fatalf("created file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "novel.md")); !os.IsNotExist(err) {
		t.Fatal("staged novel.md should have been removed")
	}
}

// Removing a routine must remove every per-routine state file, subdirectories
// included: a leftover trigger baseline means a re-created routine with the
// same name never fires on its first genuine change, and a leftover cursor
// replays or skips an inbox.
func TestRemoveRoutineStateCoversAllSubtrees(t *testing.T) {
	dir := t.TempDir()
	mem := At(dir)
	sd := mem.StateDir()
	for _, p := range []string{
		filepath.Join(sd, "x.json"),
		filepath.Join(sd, "triggers", "x.json"),
		filepath.Join(sd, "cursors", "x.json"),
		filepath.Join(sd, "y.json"),
		filepath.Join(sd, "triggers", "y.json"),
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("{}"), 0o644)
	}

	removed, err := mem.RemoveRoutineState("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("expected 3 removed paths, got %v", removed)
	}
	for _, p := range []string{
		filepath.Join(sd, "x.json"),
		filepath.Join(sd, "triggers", "x.json"),
		filepath.Join(sd, "cursors", "x.json"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", p)
		}
	}
	for _, p := range []string{filepath.Join(sd, "y.json"), filepath.Join(sd, "triggers", "y.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should survive: %v", p, err)
		}
	}

	// Idempotent, and quiet when there is no state at all.
	if removed, err := mem.RemoveRoutineState("x"); err != nil || len(removed) != 0 {
		t.Fatalf("second removal: %v, %v", removed, err)
	}
	if removed, err := At(t.TempDir()).RemoveRoutineState("x"); err != nil || removed != nil {
		t.Fatalf("no state dir: %v, %v", removed, err)
	}
}

// A second clone (a new container generation) must adopt the existing memory
// branch from origin instead of minting a fresh root.
func TestEnsureWorktreeAdoptsOriginBranch(t *testing.T) {
	base := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := filepath.Join(base, "origin.git")
	run(base, "git", "init", "-q", "-b", "main", "--bare", bare)

	// Generation 1: create memory, write a fact, push.
	a := filepath.Join(base, "a")
	run(base, "git", "clone", "-q", bare, a)
	os.WriteFile(filepath.Join(a, "x.txt"), []byte("x"), 0o644)
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A")
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "main")
	run(a, "git", "push", "-q", "origin", "main")
	if err := At(a).Ensure(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "memory", "events.md"), []byte("generation one fact\n"), 0o644)
	if _, err := At(a).Commit("Fact from generation one"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}

	// Generation 2: fresh clone (no local memory branch), must adopt.
	b := filepath.Join(base, "b")
	run(base, "git", "clone", "-q", bare, b)
	if err := At(b).Ensure(); err != nil {
		t.Fatal(err)
	}
	log := run(filepath.Join(b, "memory"), "git", "log", "--oneline")
	if !strings.Contains(log, "Fact from generation one") {
		t.Fatalf("generation two did not adopt origin history: %q", log)
	}
	if got := strings.Count(log, "Memory branch root"); got != 1 {
		t.Fatalf("expected exactly 1 root commit, got %d: %q", got, log)
	}
	events, _ := os.ReadFile(filepath.Join(b, "memory", "events.md"))
	if !strings.Contains(string(events), "generation one fact") {
		t.Fatalf("adopted events missing: %q", events)
	}
}

// Supervisor-written entries are committed and pushed, so a secret quoted in
// a git or provider error is a durable, published record -- redaction belongs
// at the append seam, not at whichever call site remembered it.
func TestSupervisorEntriesRedactSecrets(t *testing.T) {
	const masterKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"    // gitleaks:allow -- synthetic fixture
	const deployKeyLine = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAt" // gitleaks:allow -- synthetic fixture
	t.Setenv("OPENROUTINES_MASTER_KEY", masterKey)
	dir := deliveryFixture(t)
	// Materializing a secret is what registers it: loading the key and
	// reading the deploy key are the only ways their values enter the
	// process, so they are the only ways the values can leak.
	if _, err := creds.LoadKey(dir); err != nil {
		t.Fatal(err)
	}
	registerDeployKey("-----BEGIN OPENSSH PRIVATE KEY-----\n" + deployKeyLine + "\n-----END OPENSSH PRIVATE KEY-----") // gitleaks:allow -- synthetic fixture

	if err := At(dir).AppendEvent("2026-07-29 supervisor: push failed with key " + deployKeyLine + " and master key " + masterKey); err != nil {
		t.Fatal(err)
	}
	if err := At(dir).AppendHumanTask("task-20260729-1", "investigate: run failed with master key "+masterKey+" (source: supervisor; added 2026-07-29)"); err != nil {
		t.Fatal(err)
	}
	if err := At(dir).AppendRunRecord(`{"run_id":"run_x","hint":"push failed with key ` + deployKeyLine + ` and master key ` + masterKey + `"}`); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"events.md", "tasks.md", "runs.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(At(dir).Worktree(), file))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{masterKey, deployKeyLine} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries an unredacted secret: %s", file, text)
			}
		}
		if !strings.Contains(text, "[REDACTED:MASTER_KEY]") {
			t.Errorf("%s missing redaction marker: %s", file, text)
		}
	}
}

func TestGitChildEnvExcludesSupervisorSecrets(t *testing.T) {
	const masterKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899" // gitleaks:allow -- synthetic fixture
	t.Setenv("OPENROUTINES_MASTER_KEY", masterKey)
	t.Setenv("OPENROUTINES_DEPLOY_KEY", "-----BEGIN OPENSSH PRIVATE KEY-----")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/certs/corporate-proxy.pem")

	cmd := newGitCmd(t.TempDir(), []string{"status"})
	defer cmd.cancel()
	env := cmd.Env

	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENROUTINES_") {
			t.Errorf("git child inherits framework variable: %q", strings.SplitN(kv, "=", 2)[0])
		}
		if strings.Contains(kv, masterKey) {
			t.Errorf("git child carries the master key: %q", strings.SplitN(kv, "=", 2)[0])
		}
	}
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_SSL_CAINFO=" + os.Getenv("GIT_SSL_CAINFO"),
	} {
		if !slices.Contains(env, want) {
			t.Errorf("git child env missing %q: %v", want, env)
		}
	}
}

func TestGitChildEnvCarriesDeployKeySSHCommand(t *testing.T) {
	prev := sshCommand
	t.Cleanup(func() { sshCommand = prev })
	sshCommand = "ssh -i /root/.ssh/openroutines_deploy"

	cmd := newGitCmd(t.TempDir(), []string{"push"})
	defer cmd.cancel()
	if !slices.Contains(cmd.Env, "GIT_SSH_COMMAND="+sshCommand) {
		t.Error("GIT_SSH_COMMAND missing from git child env")
	}
}

// originRepo is a repository whose only interesting property is its origin
// URL. `ls-remote --get-url` is git's own answer to "what would you connect
// to", insteadOf rewriting included, and it touches no network.
func originRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", origin)
	return dir
}

func resolvedOrigin(t *testing.T, dir string) string {
	t.Helper()
	out, err := git(dir, "ls-remote", "--get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withDeployKey(t *testing.T) {
	t.Helper()
	prevSSH, prevRewrite := sshCommand, originRewrite
	t.Cleanup(func() { sshCommand, originRewrite = prevSSH, prevRewrite })
	sshCommand = "ssh -i /root/.ssh/openroutines_deploy"
	originRewrite = nil
}

func TestOriginRewriteRoutesHTTPSOriginThroughDeployKey(t *testing.T) {
	withDeployKey(t)
	dir := originRepo(t, "https://github.com/acme/agent.git")

	ConfigureOriginRewrite(dir)

	if got, want := resolvedOrigin(t, dir), "git@github.com:acme/agent.git"; got != want {
		t.Errorf("origin resolves to %q, want %q", got, want)
	}
}

func TestOriginRewriteLeavesOriginsTheDeployKeyCannotServe(t *testing.T) {
	for _, origin := range []string{
		"git@github.com:acme/agent.git",
		"ssh://git@github.com/acme/agent.git",
		"https://user:token@github.com/acme/agent.git",
		"https://git.acme.test:8443/acme/agent.git",
		"/srv/git/agent.git",
	} {
		t.Run(origin, func(t *testing.T) {
			withDeployKey(t)
			dir := originRepo(t, origin)

			ConfigureOriginRewrite(dir)

			if got := resolvedOrigin(t, dir); got != origin {
				t.Errorf("origin resolves to %q, want it untouched (%q)", got, origin)
			}
		})
	}
}

func TestOriginRewriteRequiresADeployKey(t *testing.T) {
	withDeployKey(t)
	sshCommand = ""
	dir := originRepo(t, "https://github.com/acme/agent.git")

	ConfigureOriginRewrite(dir)

	if got := resolvedOrigin(t, dir); got != "https://github.com/acme/agent.git" {
		t.Errorf("origin rewritten without a deploy key to authenticate it: %q", got)
	}
}

// The import is a three-way merge against the run's base snapshot; these are
// the decisions it can make, tested directly because the wrong one is silent
// (design decision "Overlap: kernel locks, skip-don't-queue").
func TestImportThreeWayMerge(t *testing.T) {
	repo := t.TempDir()
	wt := At(repo).Worktree()
	os.MkdirAll(wt, 0o755)
	staging, base := t.TempDir(), t.TempDir()
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// stale.md: untouched by the run while a concurrent settlement moved the
	// worktree -- the stale staged copy must not regress it.
	write(base, "stale.md", "v1\n")
	write(staging, "stale.md", "v1\n")
	write(wt, "stale.md", "v1\n- concurrent\n")
	// mine.md: only this run changed it -- the staged copy lands whole.
	write(base, "mine.md", "a\n")
	write(staging, "mine.md", "a\nb\n")
	write(wt, "mine.md", "a\n")
	// both.md: both runs appended -- the verified append merge keeps both.
	write(base, "both.md", "x\n")
	write(staging, "both.md", "x\ntheirs\n")
	write(wt, "both.md", "x\nours\n")
	// semantic.md: both runs rewrote the same fact -- canonical stays valid
	// and the competing version is quarantined for human resolution.
	write(base, "semantic.md", "status: idle\n")
	write(staging, "semantic.md", "status: away\n")
	write(wt, "semantic.md", "status: shipping\n")
	// gone.md: the run deleted it and nothing moved underneath -- deleted.
	write(base, "gone.md", "old\n")
	write(wt, "gone.md", "old\n")
	// contested.md: the run deleted it but a concurrent settlement wrote to
	// it -- the deletion loses.
	write(base, "contested.md", "old\n")
	write(wt, "contested.md", "old\n- news\n")

	conflicted, err := At(repo).Import(staging, base)
	if err != nil {
		t.Fatal(err)
	}
	got := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(wt, name))
		return string(raw)
	}
	if got("stale.md") != "v1\n- concurrent\n" {
		t.Errorf("stale.md regressed to the staged copy: %q", got("stale.md"))
	}
	if got("mine.md") != "a\nb\n" {
		t.Errorf("mine.md = %q, want the staged change", got("mine.md"))
	}
	if b := got("both.md"); !strings.Contains(b, "ours") || !strings.Contains(b, "theirs") {
		t.Errorf("both.md = %q, want both sides kept", b)
	}
	if len(conflicted) != 1 || conflicted[0].Path != "semantic.md" {
		t.Errorf("conflicted = %v, want semantic.md quarantined", conflicted)
	} else if raw, err := os.ReadFile(filepath.Join(wt, conflicted[0].Quarantine)); err != nil || string(raw) != "status: away\n" {
		t.Errorf("quarantine = %q, %v; want competing semantic edit", raw, err)
	}
	if got("semantic.md") != "status: shipping\n" {
		t.Errorf("semantic.md = %q, want last valid canonical version", got("semantic.md"))
	}
	if _, err := os.Stat(filepath.Join(wt, "gone.md")); !os.IsNotExist(err) {
		t.Error("gone.md should be deleted: the worktree still matched the base")
	}
	if got("contested.md") != "old\n- news\n" {
		t.Errorf("contested.md = %q, want the concurrent write kept over the deletion", got("contested.md"))
	}
}
