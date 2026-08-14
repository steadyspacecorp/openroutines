// Package repository owns runtime Git execution, authentication, and the
// Git-backed supervisor lease.
package repository

import (
	"fmt"
	"log/slog"
)

type Repository struct {
	dir       string
	remote    bool
	inspected bool
}

func Open(dir string) *Repository { return &Repository{dir: dir} }

func (repo *Repository) Dir() string { return repo.dir }

func Initialize(dir string) (*Repository, error) {
	if err := initialize(dir); err != nil {
		return nil, err
	}
	return Open(dir), nil
}

func initialize(dir string) error {
	_, err := git(dir, "init", "--quiet", "--initial-branch=main")
	return err
}

func (repo *Repository) CommitAll(message string) error {
	if _, err := repo.Run("add", "-A"); err != nil {
		return err
	}
	_, err := repo.Run("commit", "--quiet", "-m", message)
	return err
}

// ConfigureAuthentication materializes the deploy key and routes a compatible
// HTTPS origin through it for this process's Git children.
func (repo *Repository) ConfigureAuthentication() error {
	configuredKey, err := ConfigureDeployKey()
	if err != nil {
		return fmt.Errorf("deploy key: %w", err)
	}
	if configuredKey && repo.configureOriginRewrite() {
		slog.Info("repository: routing the HTTPS origin through the deploy key")
	}
	if !repo.Remote() {
		slog.Warn("repository has no origin -- remote persistence and the supervisor lease are disabled (local mode)")
	}
	return nil
}

func (repo *Repository) Remote() bool {
	if !repo.inspected {
		_, repo.remote = repo.Origin()
	}
	return repo.remote
}

func (repo *Repository) Origin() (string, bool) {
	origin, err := repo.Run("remote", "get-url", "origin")
	repo.remote, repo.inspected = err == nil, true
	return Display(origin), repo.remote
}
