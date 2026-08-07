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
)

// OriginSnapshot is one immutable, exported view of origin/knowledge. Close
// removes its temporary tree; it never materializes or changes knowledge/.
type OriginSnapshot struct {
	Dir       string
	Commit    string
	FetchedAt time.Time
	repoDir   string
}

// SnapshotFile is one regular file in a snapshot.
type SnapshotFile struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size_bytes"`
	LastChanged time.Time `json:"last_changed"`
}

// SnapshotStats describes the checked-out tree and its reachable history.
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

// SnapshotRelation describes how a materialized local worktree relates to a
// fetched origin snapshot.
type SnapshotRelation struct {
	Materialized bool
	Behind       int
	Ahead        int
	Diverged     bool
	Uncommitted  int
}

// FetchOriginSnapshot fetches origin/knowledge and exports its current tree to
// a temporary directory. Unlike Sync it neither reads nor changes the local
// knowledge worktree.
func (store *Store) FetchOriginSnapshot() (*OriginSnapshot, error) {
	if !store.HasOrigin() {
		return nil, errors.New("no origin remote -- knowledge exists locally only")
	}
	if _, err := git(store.repoDir, "fetch", "--quiet", "origin", Branch); err != nil {
		return nil, fmt.Errorf("fetching origin/%s: %w", Branch, err)
	}
	commit, err := git(store.repoDir, "rev-parse", "refs/remotes/origin/"+Branch)
	if err != nil {
		return nil, fmt.Errorf("origin has no %s branch", Branch)
	}
	dir, err := os.MkdirTemp("", "openroutines-knowledge-*")
	if err != nil {
		return nil, err
	}
	if _, err := git(store.repoDir, "--work-tree="+dir, "checkout", "--quiet", commit, "--", "."); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("exporting origin/%s: %w", Branch, err)
	}
	return &OriginSnapshot{Dir: dir, Commit: commit, FetchedAt: time.Now(), repoDir: store.repoDir}, nil
}

// Close removes the exported temporary tree.
func (s *OriginSnapshot) Close() error { return os.RemoveAll(s.Dir) }

// Relation compares the local knowledge worktree with this snapshot without
// changing either one.
func (s *OriginSnapshot) Relation(store *Store) SnapshotRelation {
	status := store.Status()
	r := SnapshotRelation{Materialized: status.Materialized, Uncommitted: status.Uncommitted}
	if !status.Materialized {
		return r
	}
	local, err := git(store.Worktree(), "rev-parse", "HEAD")
	if err != nil || local == s.Commit {
		return r
	}
	if isAncestor(s.repoDir, local, s.Commit) {
		out, _ := git(s.repoDir, "rev-list", "--count", local+".."+s.Commit)
		r.Behind, _ = strconv.Atoi(out)
		return r
	}
	if isAncestor(s.repoDir, s.Commit, local) {
		out, _ := git(s.repoDir, "rev-list", "--count", s.Commit+".."+local)
		r.Ahead, _ = strconv.Atoi(out)
		return r
	}
	r.Diverged = true
	return r
}

// Files returns every regular file at or below rel. Paths always use slashes.
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

// ReadFile reads one regular file beneath the snapshot.
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
	out, err := git(s.repoDir, "log", "-1", "--format=%cI", s.Commit, "--", path)
	if err != nil || out == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, out)
}

// Stats computes facts from the snapshot tree and reachable branch history.
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
	last, err := git(s.repoDir, "show", "-s", "--format=%cI%n%s", s.Commit)
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
	root, err := git(s.repoDir, "rev-list", "--max-parents=0", s.Commit)
	if err != nil {
		return SnapshotStats{}, err
	}
	root = strings.Split(root, "\n")[0]
	first, err := git(s.repoDir, "show", "-s", "--format=%cI", root)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.FirstWrite, err = time.Parse(time.RFC3339, first)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.HistoryDays = max(0, int(st.LastWrite.Sub(st.FirstWrite).Hours()/24))
	count, err := git(s.repoDir, "rev-list", "--count", s.Commit)
	if err != nil {
		return SnapshotStats{}, err
	}
	st.Commits, _ = strconv.Atoi(count)
	return st, nil
}
