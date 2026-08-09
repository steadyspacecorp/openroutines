// Acquires a contained, revision-pinned tree from Git.
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	fetchTimeout     = 5 * time.Minute
	gitKillGrace     = 2 * time.Second
	gitDrainDeadline = 5 * time.Second
)

// Identifies the repository content returned by Fetch.
type Provenance struct {
	Repository string
	Path       string
	Revision   string
}

// Clones a source reference and returns its contained subpath at revision.
func Fetch(sourceRef, subPath, revision string) (root string, provenance Provenance, cleanup func(), err error) {
	cleanup = func() {}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	repository := sourceRef
	cloneURL := ""
	if fi, statErr := os.Stat(sourceRef); statErr == nil && fi.IsDir() {
		abs, err := filepath.Abs(sourceRef)
		if err != nil {
			return "", Provenance{}, cleanup, err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return "", Provenance{}, cleanup, err
		}
		out, err := gitOutput(ctx, "-C", abs, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", Provenance{}, cleanup, fmt.Errorf("local source must be inside a git repository: %w", err)
		}
		repoRoot, err := filepath.EvalSymlinks(strings.TrimSpace(out))
		if err != nil {
			return "", Provenance{}, cleanup, err
		}
		cloneURL = repoRoot
		localPath, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			return "", Provenance{}, cleanup, err
		}
		if subPath != "" {
			localPath = filepath.Join(localPath, subPath)
		}
		subPath = filepath.ToSlash(localPath)
		repository = repoRoot
	}
	if cloneURL == "" {
		cloneURL = repository
	}
	if shorthand(repository) {
		cloneURL = "https://github.com/" + repository + ".git"
		repository = cloneURL
	}

	tmp, err := os.MkdirTemp("", "openroutines-source-*")
	if err != nil {
		return "", Provenance{}, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	if _, err := gitOutput(ctx, "-c", "protocol.ext.allow=never", "clone", "--quiet", "--", cloneURL, tmp); err != nil {
		cleanup()
		return "", Provenance{}, func() {}, fmt.Errorf("clone %s: %w", cloneURL, err)
	}
	if revision != "" {
		if _, err := gitOutput(ctx, "-C", tmp, "checkout", "--quiet", "--detach", revision); err != nil {
			cleanup()
			return "", Provenance{}, func() {}, fmt.Errorf("checkout %s: %w", revision, err)
		}
	}
	revision, err = gitOutput(ctx, "-C", tmp, "rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return "", Provenance{}, func() {}, fmt.Errorf("resolve fetched revision: %w", err)
	}
	root = tmp
	if subPath != "" && subPath != "." {
		root, err = ResolvePath(tmp, filepath.FromSlash(subPath))
		if err != nil {
			cleanup()
			return "", Provenance{}, func() {}, err
		}
	}
	return root, Provenance{Repository: repository, Path: subPath, Revision: revision}, cleanup, nil
}

// Resolves subPath while refusing traversal and symlink escapes.
func ResolvePath(root, subPath string) (string, error) {
	clean := filepath.Clean(subPath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--path %q escapes the source repository", subPath)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--path %q escapes the source repository", subPath)
	}
	return candidate, nil
}

func shorthand(repository string) bool {
	return !strings.Contains(repository, "://") && !strings.Contains(repository, "@") && strings.Count(repository, "/") == 1
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGitGroup(cmd.Process.Pid) }
	cmd.WaitDelay = gitDrainDeadline
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			return "", fmt.Errorf("git timed out: %w: %s", ctx.Err(), detail)
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			return "", fmt.Errorf("git left its output pipe open after %s: %w", gitDrainDeadline, err)
		}
		if detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func killGitGroup(pid int) error {
	group := -pid
	if err := syscall.Kill(group, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	time.Sleep(gitKillGrace)
	if err := syscall.Kill(group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
