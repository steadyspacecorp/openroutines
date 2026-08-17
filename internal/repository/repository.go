package repository

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const runtimeOriginMarker = "openroutines-origin"

type Repository struct {
	dir    string
	remote bool
}

func Open(dir string) *Repository { return &Repository{dir: dir} }

func (repo *Repository) Dir() string { return repo.dir }

func Initialize(dir string) error {
	return initialize(dir)
}

func initialize(dir string) error {
	_, err := git(dir, "init", "--quiet", "--initial-branch=main")
	return err
}

func (repo *Repository) Prepare(reference string, deployed bool) error {
	if !deployed {
		if !repo.Remote() {
			slog.Warn("repository has no origin -- remote persistence and the supervisor lease are disabled (local mode)")
		}
		return nil
	}
	if reference == "" {
		return fmt.Errorf("repo is required in deployed containers")
	}
	origin, err := GitOrigin(reference)
	if err != nil {
		return err
	}
	configuredKey, err := configureDeployKey(repo.dir)
	if err != nil {
		return fmt.Errorf("deploy key: %w", err)
	}
	if !configuredKey {
		return fmt.Errorf("deploy key is required in deployed containers: set %s or %s, or mount %s in the agent root", EnvDeployKey, EnvDeployKeyFile, DeployKeyFileName)
	}
	prepared, err := repo.prepared(origin)
	if err != nil {
		return err
	}
	if prepared {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(repo.dir, ".git")); err != nil {
		return fmt.Errorf("removing provider git metadata: %w", err)
	}
	repo.remote = false
	if err := initialize(repo.dir); err != nil {
		return fmt.Errorf("initializing runtime repository: %w", err)
	}
	if _, err := repo.Run("remote", "add", "origin", origin); err != nil {
		return fmt.Errorf("configuring runtime git origin: %w", err)
	}
	if err := os.WriteFile(repo.runtimeOriginMarker(), []byte(origin+"\n"), 0o644); err != nil {
		return fmt.Errorf("marking runtime git repository: %w", err)
	}
	repo.remote = true
	return nil
}

func (repo *Repository) prepared(origin string) (bool, error) {
	raw, err := os.ReadFile(repo.runtimeOriginMarker())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading runtime git repository marker: %w", err)
	}
	marked := strings.TrimSpace(string(raw))
	current, ok := repo.origin()
	if !ok || marked != origin || current != origin {
		return false, fmt.Errorf("runtime git repository does not match repo %q -- refusing to replace local Git state", redactOrigin(origin))
	}
	return true, nil
}

func (repo *Repository) runtimeOriginMarker() string {
	return filepath.Join(repo.dir, ".git", runtimeOriginMarker)
}

func (repo *Repository) Remote() bool {
	if !repo.remote {
		_, repo.remote = repo.Origin()
	}
	return repo.remote
}

func (repo *Repository) Origin() (string, bool) {
	origin, ok := repo.origin()
	return redactOrigin(origin), ok
}

func (repo *Repository) origin() (string, bool) {
	origin, err := repo.Run("remote", "get-url", "origin")
	repo.remote = err == nil
	return origin, repo.remote
}

func redactOrigin(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		if strings.Contains(value, "://") {
			return ""
		}
		return value
	}
	if u.User == nil {
		return value
	}
	username := u.User.Username()
	_, password := u.User.Password()
	if u.Scheme == "http" || u.Scheme == "https" {
		u.User = nil
	} else if password {
		u.User = url.User(username)
	}
	return u.String()
}
