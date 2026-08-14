package repository

// Worktree is a Git working directory attached to a Repository.
type Worktree struct {
	dir string
}

func (repo *Repository) Worktree(dir string) *Worktree {
	return &Worktree{dir: dir}
}

func (worktree *Worktree) Dir() string { return worktree.dir }

func (repo *Repository) Run(args ...string) (string, error) {
	return git(repo.dir, args...)
}

func (repo *Repository) RunEnv(env []string, args ...string) (string, error) {
	return gitEnv(repo.dir, env, args...)
}

func (repo *Repository) RunStdin(stdin string, args ...string) (string, error) {
	return gitStdin(repo.dir, stdin, args...)
}

func (worktree *Worktree) Run(args ...string) (string, error) {
	return git(worktree.dir, args...)
}
