package knowledge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/repository"
)

type OriginSnapshot struct {
	Dir       string
	Commit    string
	FetchedAt time.Time
	repo      *repository.Repository
}

type SnapshotFile struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size_bytes"`
	LastChanged time.Time `json:"last_changed"`
}

type SnapshotStats struct {
	Commit       string    `json:"commit"`
	FetchedAt    time.Time `json:"fetched_at"`
	FirstWrite   time.Time `json:"first_write"`
	LastWrite    time.Time `json:"last_write"`
	LastSubject  string    `json:"last_subject"`
	HistoryDays  int       `json:"history_days"`
	SizeBytes    int64     `json:"size_bytes"`
	Files        int       `json:"files"`
	Commits      int       `json:"commits"`
	LargestPath  string    `json:"largest_path,omitempty"`
	LargestBytes int64     `json:"largest_bytes"`
}

type SnapshotRelation struct {
	Materialized bool
	Behind       int
	Ahead        int
	Diverged     bool
	Uncommitted  int
}

func (store *Store) FetchOriginSnapshot() (*OriginSnapshot, error) {
	if !store.repo.Remote() {
		return nil, errors.New("no origin remote -- knowledge exists locally only")
	}
	if _, err := store.repo.Run("fetch", "--quiet", "origin", Branch); err != nil {
		return nil, fmt.Errorf("fetching origin/%s: %w", Branch, err)
	}
	commit, err := store.repo.Run("rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return nil, fmt.Errorf("origin has no %s branch", Branch)
	}
	dir, err := os.MkdirTemp("", "openroutines-knowledge-*")
	if err != nil {
		return nil, err
	}

	// checkout <commit> -- <paths> writes whatever index it finds -- the agent
	// repository's own, which would leave the whole branch staged there. A
	// scratch index keeps the read-only export read-only.
	index := filepath.Join(dir, ".openroutines-export-index")
	if _, err := store.repo.RunEnv([]string{"GIT_INDEX_FILE=" + index}, "--work-tree="+dir, "checkout", "--quiet", commit, "--", "."); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("exporting origin/%s: %w", Branch, err)
	}
	if err := os.Remove(index); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("exporting origin/%s: %w", Branch, err)
	}
	return &OriginSnapshot{Dir: dir, Commit: commit, FetchedAt: time.Now(), repo: store.repo}, nil
}

func (s *OriginSnapshot) Close() error { return os.RemoveAll(s.Dir) }

func (s *OriginSnapshot) ChangesSince(cutoff time.Time) (string, error) {
	args := append([]string{
		"log", "--reverse", "--date=iso-strict",
		"--format=commit %H%nwhen %cI%nsubject %s", "-p", "-U1", "--no-color",
		"--since=" + cutoff.Format(time.RFC3339), s.Commit, "--", ".",
	}, deliveryExcludes...)
	out, err := s.repo.Run(args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "No shared knowledge changes were committed in this window.\n", nil
	}
	return out + "\n", nil
}

func (s *OriginSnapshot) Relation(store *Store) SnapshotRelation {
	status := store.Status()
	r := SnapshotRelation{Materialized: status.Materialized, Uncommitted: status.Uncommitted}
	if !status.Materialized {
		return r
	}
	local, err := store.worktree.Run("rev-parse", "HEAD")
	if err != nil || local == s.Commit {
		return r
	}
	if behind, _ := s.repo.IsAncestor(local, s.Commit); behind {
		out, _ := s.repo.Run("rev-list", "--count", local+".."+s.Commit)
		r.Behind, _ = strconv.Atoi(out)
		return r
	}
	if ahead, _ := s.repo.IsAncestor(s.Commit, local); ahead {
		out, _ := s.repo.Run("rev-list", "--count", s.Commit+".."+local)
		r.Ahead, _ = strconv.Atoi(out)
		return r
	}
	r.Diverged = true
	return r
}

func (s *OriginSnapshot) Files(rel string) ([]SnapshotFile, error) {
	root, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	var out []SnapshotFile
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("knowledge snapshot contains symbolic link %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("knowledge snapshot contains non-regular file %s", path)
		}
		relPath, _ := filepath.Rel(s.Dir, path)
		relPath = filepath.ToSlash(relPath)
		changed, _ := s.fileChanged(relPath)
		out = append(out, SnapshotFile{Path: relPath, Size: info.Size(), LastChanged: changed})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *OriginSnapshot) ReadFile(rel string) ([]byte, error) {
	path, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return os.ReadFile(path)
}

func (s *OriginSnapshot) resolve(rel string) (string, error) {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return s.Dir, nil
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must stay inside knowledge: %q", rel)
	}
	path := s.Dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("knowledge snapshot contains symbolic link %s", rel)
		}
	}
	return path, nil
}

func (s *OriginSnapshot) fileChanged(path string) (time.Time, error) {
	out, err := s.repo.Run("log", "-1", "--format=%cI", s.Commit, "--", path)
	if err != nil || out == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, out)
}

func (s *OriginSnapshot) Stats() (SnapshotStats, error) {
	files, err := s.Files("")
	if err != nil {
		return SnapshotStats{}, err
	}
	st := SnapshotStats{Commit: s.Commit, FetchedAt: s.FetchedAt, Files: len(files)}
	for _, f := range files {
		st.SizeBytes += f.Size
		if f.Size > st.LargestBytes {
			st.LargestPath, st.LargestBytes = f.Path, f.Size
		}
	}
	last, err := s.repo.Run("show", "-s", "--format=%cI%n%s", s.Commit)
	if err != nil {
		return SnapshotStats{}, err
	}
	parts := strings.SplitN(last, "\n", 2)
	st.LastWrite, err = time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return SnapshotStats{}, err
	}
	if len(parts) == 2 {
		st.LastSubject = parts[1]
	}
	root, err := s.repo.Run("rev-list", "--max-parents=0", s.Commit)
	if err != nil {
		return SnapshotStats{}, err
	}
	root = strings.Split(root, "\n")[0]
	first, err := s.repo.Run("show", "-s", "--format=%cI", root)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.FirstWrite, err = time.Parse(time.RFC3339, first)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.HistoryDays = max(0, int(st.LastWrite.Sub(st.FirstWrite).Hours()/24))
	count, err := s.repo.Run("rev-list", "--count", s.Commit)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.Commits, _ = strconv.Atoi(count)
	return st, nil
}
