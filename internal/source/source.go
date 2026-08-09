// Package source acquires a contained, revision-pinned tree from Git.
package source

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const fetchTimeout = 5 * time.Minute

// Provenance identifies the repository content returned by Fetch.
type Provenance struct {
	Repository string
	Path       string
	Revision   string
}

// Fetch clones source and returns its contained subpath at revision.
func Fetch(source, subPath, revision string) (root string, provenance Provenance, cleanup func(), err error) {
	cleanup = func() {}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	repository := source
	cloneURL := ""
	if fi, statErr := os.Stat(source); statErr == nil && fi.IsDir() {
		abs, err := filepath.Abs(source)
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

// ResolvePath resolves subPath while refusing traversal and symlink escapes.
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
