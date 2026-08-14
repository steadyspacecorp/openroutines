// Package repository owns runtime Git execution, authentication, and the
// Git-backed supervisor lease.
package repository

import (
	"fmt"
	"log/slog"
)

type Repository struct {
	dir string
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
	if _, err := Run(repo.dir, "add", "-A"); err != nil {
		return err
	}
	_, err := Run(repo.dir, "commit", "--quiet", "-m", message)
	return err
}

// ConfigureAuthentication materializes the deploy key and routes a compatible
// HTTPS origin through it for this process's Git children.
func (repo *Repository) ConfigureAuthentication() error {
	configuredKey, err := ConfigureDeployKey()
	if err != nil {
		return fmt.Errorf("deploy key: %w", err)
	}
	if configuredKey && ConfigureOriginRewrite(repo.dir) {
		slog.Info("repository: routing the HTTPS origin through the deploy key")
	}
	return nil
}
