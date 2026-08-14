package repository

import "strings"

func (repo *Repository) RemoteRef(ref string) (sha string, exists bool, err error) {
	out, err := repo.Run("ls-remote", "--exit-code", "origin", ref)
	if err != nil {
		if gitExitCode(err) == 2 {
			return "", false, nil
		}
		return "", false, err
	}
	sha, _, _ = strings.Cut(out, "\t")
	return sha, true, nil
}

func resolveCommit(dir, revision string) (sha string, exists bool, err error) {
	sha, err = git(dir, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil {
		if gitExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return sha, true, nil
}

func isAncestor(dir, ancestor, descendant string) (bool, error) {
	_, err := git(dir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if gitExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func (repo *Repository) ResolveCommit(revision string) (string, bool, error) {
	return resolveCommit(repo.dir, revision)
}

func (repo *Repository) IsAncestor(ancestor, descendant string) (bool, error) {
	return isAncestor(repo.dir, ancestor, descendant)
}

func (worktree *Worktree) ResolveCommit(revision string) (string, bool, error) {
	return resolveCommit(worktree.dir, revision)
}

func (worktree *Worktree) IsAncestor(ancestor, descendant string) (bool, error) {
	return isAncestor(worktree.dir, ancestor, descendant)
}
