package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fetch acquires a plugin source: a local directory, a git URL, or an
// owner/repo GitHub shorthand. Both go through a temporary clone, so the
// returned bundle root always corresponds to the recorded provenance.
// revision pins the checkout; empty means the clone's head.
func Fetch(source, subPath, revision string) (root string, prov Source, cleanup func(), err error) {
	cleanup = func() {}
	repository := source
	cloneURL := ""
	if fi, err := os.Stat(source); err == nil && fi.IsDir() {
		abs, err := filepath.Abs(source)
		if err != nil {
			return "", Source{}, cleanup, err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return "", Source{}, cleanup, err
		}
		out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "", Source{}, cleanup, fmt.Errorf("local plugin source must be inside a git repository: %w", err)
		}
		repoRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			return "", Source{}, cleanup, err
		}
		cloneURL = repoRoot
		localPath, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			return "", Source{}, cleanup, err
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
	if !strings.Contains(repository, "://") && !strings.Contains(repository, "@") && strings.Count(repository, "/") == 1 {
		cloneURL = "https://github.com/" + repository + ".git"
		repository = cloneURL
	}
	tmp, err := os.MkdirTemp("", "openroutines-plugin-*")
	if err != nil {
		return "", Source{}, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	clone := exec.Command("git", "-c", "protocol.ext.allow=never", "clone", "--quiet", "--", cloneURL, tmp)
	var cloneErr strings.Builder
	clone.Stderr = &cloneErr
	if err := clone.Run(); err != nil {
		cleanup()
		return "", Source{}, func() {}, fmt.Errorf("clone %s: %w\n%s", cloneURL, err, strings.TrimSpace(cloneErr.String()))
	}
	if revision != "" {
		checkout := exec.Command("git", "-C", tmp, "checkout", "--quiet", "--detach", revision)
		var checkoutErr strings.Builder
		checkout.Stderr = &checkoutErr
		if err := checkout.Run(); err != nil {
			cleanup()
			return "", Source{}, func() {}, fmt.Errorf("checkout %s: %w\n%s", revision, err, strings.TrimSpace(checkoutErr.String()))
		}
	}
	revBytes, err := exec.Command("git", "-C", tmp, "rev-parse", "HEAD").Output()
	if err != nil {
		cleanup()
		return "", Source{}, func() {}, err
	}
	root = tmp
	if subPath != "" && subPath != "." {
		root, err = subdir(tmp, filepath.FromSlash(subPath))
		if err != nil {
			cleanup()
			return "", Source{}, func() {}, err
		}
	}
	return root, Source{Repository: repository, Path: subPath, Revision: strings.TrimSpace(string(revBytes))}, cleanup, nil
}

// Resolves a --path inside the clone, refusing traversal or symlink
// escapes out of the repository.
func subdir(root, subPath string) (string, error) {
	clean := filepath.Clean(subPath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--path %q escapes the plugin repository", subPath)
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
		return "", fmt.Errorf("--path %q escapes the plugin repository", subPath)
	}
	return candidate, nil
}
