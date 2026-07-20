package memory

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// SyncReport describes what happened when reconciling with origin.
type SyncReport struct {
	NoOrigin      bool   // repo has no origin remote (local mode)
	RemoteMissing bool   // origin exists but has no memory branch yet
	Unreachable   bool   // origin exists but could not be contacted
	Rewritten     bool   // remote history was rewritten -- sync refused
	Conflict      bool   // rebase conflict -- sync refused
	Adopted       bool   // remote commits were adopted locally
	Detail        string // human-readable context for blockers/logs
}

// HasOrigin reports whether the repo has an origin remote configured.
func HasOrigin(repoDir string) bool {
	_, err := git(repoDir, "remote", "get-url", "origin")
	return err == nil
}

// Sync reconciles the local memory branch with origin, defensively
// (DESIGN.md "Memory"): fast-forward when behind; rebase local commits when
// diverged (append-only files rebase cleanly); refuse rewritten remote
// history and conflicts -- never resolve silently.
func Sync(repoDir string) SyncReport {
	wt := WorktreePath(repoDir)
	if !HasOrigin(repoDir) {
		return SyncReport{NoOrigin: true}
	}
	if _, err := git(repoDir, "ls-remote", "--exit-code", "origin", "refs/heads/"+Branch); err != nil {
		if strings.Contains(err.Error(), "exit status 2") {
			return SyncReport{RemoteMissing: true} // first push will create it
		}
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}

	// Remember the last-known remote tip before fetching: if the new tip does
	// not descend from it, someone rewrote history out from under us.
	oldTip, _ := git(repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+Branch)
	if _, err := git(repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	newTip, err := git(repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return SyncReport{Unreachable: true, Detail: err.Error()}
	}
	if oldTip != "" && oldTip != newTip && !isAncestor(repoDir, oldTip, newTip) {
		return SyncReport{Rewritten: true, Detail: fmt.Sprintf("origin/%s rewritten: %s no longer reachable from %s", Branch, short(oldTip), short(newTip))}
	}

	local, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return SyncReport{Detail: err.Error()}
	}
	switch {
	case local == newTip:
		return SyncReport{}
	case isAncestor(repoDir, local, newTip):
		// Behind: fast-forward only.
		if _, err := git(wt, "merge", "--ff-only", "--quiet", newTip); err != nil {
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		return SyncReport{Adopted: true}
	case isAncestor(repoDir, newTip, local):
		return SyncReport{} // ahead: the next push carries it
	default:
		// Diverged (human curation raced local commits): rebase ours on top.
		if _, err := git(wt, "rebase", "--quiet", newTip); err != nil {
			_, _ = git(wt, "rebase", "--abort")
			return SyncReport{Conflict: true, Detail: err.Error()}
		}
		return SyncReport{Adopted: true}
	}
}

// Push publishes the memory branch. Fast-forward only: rejections are
// reported, never forced.
func Push(repoDir string) error {
	if !HasOrigin(repoDir) {
		return nil
	}
	_, err := git(WorktreePath(repoDir), "push", "--quiet", "origin", Branch)
	return err
}

func isAncestor(repoDir, a, b string) bool {
	cmdOut, err := git(repoDir, "merge-base", "--is-ancestor", a, b)
	_ = cmdOut
	return err == nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// --- Lease: "one writer" enforced, not assumed -------------------------------

const (
	leaseRef = "refs/openroutines/lease"
	LeaseTTL = 5 * time.Minute
)

// leaseContent formats the heartbeat blob.
func leaseContent(instanceID string, now time.Time) string {
	return fmt.Sprintf("%s %d\n", instanceID, now.Unix())
}

// Lease is the current holder's heartbeat, plus the blob SHA used as the
// compare-and-swap token for atomic takeover.
type Lease struct {
	Holder string
	At     time.Time
	SHA    string
}

// ReadLease fetches the current lease from origin; nil when none exists.
func ReadLease(repoDir string) (*Lease, error) {
	if _, lerr := git(repoDir, "fetch", "--quiet", "origin", "+"+leaseRef+":"+leaseRef); lerr != nil {
		if strings.Contains(lerr.Error(), "couldn't find remote ref") {
			return nil, nil
		}
		return nil, lerr
	}
	sha, err := git(repoDir, "rev-parse", leaseRef)
	if err != nil {
		return nil, err
	}
	content, cerr := git(repoDir, "cat-file", "blob", leaseRef)
	if cerr != nil {
		return nil, cerr
	}
	fields := strings.Fields(content)
	if len(fields) != 2 {
		return nil, fmt.Errorf("malformed lease %q", content)
	}
	var unix int64
	if _, serr := fmt.Sscanf(fields[1], "%d", &unix); serr != nil {
		return nil, serr
	}
	return &Lease{Holder: fields[0], At: time.Unix(unix, 0), SHA: sha}, nil
}

// WriteLease publishes this instance's heartbeat atomically: the push
// succeeds only if origin's lease is still exactly expectedSHA (empty means
// "must not exist"). Two instances racing for the same lease cannot both
// win. Returns the new lease SHA for the next renewal's expectation.
func WriteLease(repoDir, instanceID string, now time.Time, expectedSHA string) (string, error) {
	blob, err := gitStdin(repoDir, leaseContent(instanceID, now), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	cas := "--force-with-lease=" + leaseRef + ":" + expectedSHA
	if _, err := git(repoDir, "push", "--quiet", cas, "origin", blob+":"+leaseRef); err != nil {
		return "", err
	}
	return blob, nil
}

// ReleaseLease removes this instance's lease from origin (best effort).
func ReleaseLease(repoDir string) {
	_, _ = git(repoDir, "push", "--quiet", "origin", ":"+leaseRef)
}

func gitStdin(dir, stdin string, args ...string) (string, error) {
	base := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=user",
	}
	cmd := newGitCmd(dir, append(base, args...))
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// InstanceID identifies this supervisor process for the lease.
func InstanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
