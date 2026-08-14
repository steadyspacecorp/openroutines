package knowledge

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/filetree"
	"github.com/steadyspacecorp/openroutines/internal/repository"
)

// Copies the knowledge worktree's files into a plain staging directory:
// regular files only, no git metadata. This staged copy is what a routine
// sees and writes as knowledge/.
func (store *Store) Snapshot(stagingDir string) error {
	wt := store.Worktree()
	return filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil || rel == "." {
			return err
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(stagingDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks etc. never travel into staging
		}
		return copyFile(path, dest)
	})
}

func topSegment(rel string) string {
	return strings.Split(rel, string(filepath.Separator))[0]
}

// Copies a snapshot tree verbatim, so the run's staged copy and the
// import's base come from one worktree read.
func CloneTree(src, dst string) error {
	return filetree.CopyRegular(src, dst, filetree.Options{Mode: filetree.DataFiles})
}

// Rejects paths that may never enter the worktree: git
// control files, supervisor-owned bookkeeping, absurd depth. Applied by
// Validate and again by the import copy -- the tree can change under the walk
// that validated it.
func stagedPathPolicy(rel string, isDir bool) error {
	switch filepath.Base(rel) {
	case ".git", ".gitattributes", ".gitmodules", ".gitignore":
		return fmt.Errorf("staged knowledge contains git control file %q -- rejected", rel)
	}
	if supervisorOwned[topSegment(rel)] {
		return fmt.Errorf("staged knowledge touches supervisor-owned path %q -- rejected", rel)
	}
	if isDir && strings.Count(rel, string(filepath.Separator)) > 8 {
		return fmt.Errorf("staged knowledge path %q too deep -- rejected", rel)
	}
	return nil
}

// Rejects a staged knowledge tree that contains anything but regular
// files under sane limits. A rejected tree fails the whole run.
func Validate(stagingDir string) error {
	entries := 0
	return filepath.WalkDir(stagingDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagingDir, path)
		if err != nil || rel == "." {
			return err
		}
		if err := stagedPathPolicy(rel, d.IsDir()); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("staged knowledge contains non-regular file %q -- rejected", rel)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("staged knowledge exceeds %d files -- rejected", maxEntries)
		}
		if info, err := d.Info(); err == nil {
			if info.Size() > maxFile {
				return fmt.Errorf("staged knowledge file %q exceeds %d bytes -- rejected", rel, maxFile)
			}
			// A hard link can alias a file outside the staging tree.
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return fmt.Errorf("staged knowledge file %q is a hard link -- rejected", rel)
			}
		}
		return nil
	})
}

// Applies the staged tree to the worktree as a three-way merge against
// the base snapshot: an untouched file imports nothing (a stale copy must
// never regress concurrent settlements), a file only the run changed copies
// whole, appends on both sides compose, and any other concurrent edit keeps
// the canonical file and quarantines the staged competitor. Deletions apply
// only where the worktree still matches the base. Caller commits.
func (store *Store) Import(stagingDir, baseDir string) (conflicted []Conflict, err error) {
	if err := Validate(stagingDir); err != nil {
		return nil, err
	}
	wt := store.Worktree()
	// Refuse to import over uncommitted human curation -- it has no reflog to
	// recover from. Supervisor-owned paths legitimately carry this attempt's
	// own in-flight bookkeeping.
	if out, err := repository.Run(wt, "status", "--porcelain"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			// repository.Run() trims the output, eating the first line's status column;
			// a path containing spaces degrades toward refusal, never toward
			// a silent import.
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			path := fields[len(fields)-1]
			if !supervisorOwned[topSegment(path)] {
				return nil, fmt.Errorf("knowledge worktree has uncommitted changes (%s) -- refusing to import over them; commit or discard (git -C %s ...) and re-run", path, Dir)
			}
		}
	}
	conflicted, err = copyStaged(stagingDir, baseDir, wt)
	if err != nil {
		return nil, err
	}
	// Apply staged deletions, but only where the worktree still matches the
	// base -- a file another run created or changed is theirs to keep.
	return conflicted, filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(wt, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || supervisorOwned[topSegment(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(stagingDir, rel)); !os.IsNotExist(err) {
			return nil
		}
		base, berr := os.ReadFile(filepath.Join(baseDir, rel))
		if berr != nil {
			return nil // the run never saw this file; it cannot delete it
		}
		cur, cerr := os.ReadFile(path)
		if cerr == nil && bytes.Equal(cur, base) {
			return os.Remove(path)
		}
		if cerr == nil {
			slog.Debug("knowledge: kept a file the run deleted -- the worktree moved since its snapshot", "path", rel)
		}
		return nil
	})
}

// Puts the base-snapshot copy of one knowledge file back into the
// staged tree -- restored to base, not the live worktree, so the import's
// unchanged-versus-base rule then skips it. The enforcement half of
// `teamwork: off`. Reports whether a staged change was discarded.
func RestoreFile(stagingDir, baseDir, name string) (bool, error) {
	want, werr := os.ReadFile(filepath.Join(baseDir, name))
	if werr != nil && !os.IsNotExist(werr) {
		return false, werr
	}
	// Confined like the import copy: a path swapped for a symlink must not
	// redirect the write out of the staging tree.
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	staged, serr := openStaged(root, name)
	if serr == nil {
		defer func() { _ = staged.Close() }()
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return false, serr
	}
	if os.IsNotExist(werr) {
		// The snapshot had no such file: the run must not create it either.
		if serr != nil {
			return false, nil
		}
		return true, root.Remove(name)
	}
	if serr == nil {
		got, err := io.ReadAll(staged)
		if err != nil {
			return false, err
		}
		if bytes.Equal(got, want) {
			return false, nil
		}
	}
	return true, root.WriteFile(name, want, 0o644)
}

// Records a semantic concurrent edit and the durable path where the
// competing staged version was preserved.
type Conflict struct {
	Path       string
	Quarantine string
}

// Brings every staged file into the worktree. Staging is not
// quiescent -- a descendant of the model process can outlive the run and
// rewrite what Validate approved -- so an os.Root confines every path and
// every check is re-applied on the descriptor being read. The copy lands in a
// scratch tree and is promoted only once the whole staged tree has passed:
// a mid-walk rejection must not leave a half-imported worktree for Settle to
// commit.
func copyStaged(stagingDir, baseDir, wt string) (conflicted []Conflict, err error) {
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	scratch, err := os.MkdirTemp(filepath.Dir(wt), ".openroutines-import-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	var dirs, files []string
	if err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.FromSlash(rel)
		if rel == ConsumeMarker {
			return nil // consume receipt for the runtime, never knowledge content
		}
		if err := stagedPathPolicy(rel, d.IsDir()); err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return os.MkdirAll(filepath.Join(scratch, rel), 0o755)
		}
		if len(files) >= maxEntries {
			return fmt.Errorf("staged knowledge exceeds %d files -- rejected", maxEntries)
		}
		files = append(files, rel)
		return copyStagedFile(root, rel, filepath.Join(scratch, rel))
	}); err != nil {
		return nil, err
	}
	for _, rel := range dirs {
		if err := os.MkdirAll(filepath.Join(wt, rel), 0o755); err != nil {
			return nil, err
		}
	}
	// The three-way decision, on trusted bytes: scratch copy, base, and
	// worktree are all supervisor-owned by now. Every file's final bytes
	// resolve into the scratch tree first; the rename-only pass below is
	// what keeps promotion all-or-nothing.
	var promote []string
	var quarantines []string
	for _, rel := range files {
		staged, err := os.ReadFile(filepath.Join(scratch, rel))
		if err != nil {
			return nil, err
		}
		base, berr := os.ReadFile(filepath.Join(baseDir, rel))
		if berr != nil && !os.IsNotExist(berr) {
			return nil, berr
		}
		if berr == nil && bytes.Equal(staged, base) {
			continue // untouched by the run: never regress what settled since
		}
		cur, cerr := os.ReadFile(filepath.Join(wt, rel))
		if cerr != nil && !os.IsNotExist(cerr) {
			return nil, cerr
		}
		if !os.IsNotExist(cerr) && !bytes.Equal(cur, base) && !bytes.Equal(cur, staged) {
			// A concurrently settled run changed the same file.
			if merged, ok := appendMerge(cur, base, staged); ok {
				if err := os.WriteFile(filepath.Join(scratch, rel), merged, 0o644); err != nil {
					return nil, err
				}
			} else {
				sum := sha256.Sum256(staged)
				quarantine := filepath.Join("state", "conflicts", fmt.Sprintf("%x", sum[:8]), rel)
				source := filepath.Join(scratch, rel)
				target := filepath.Join(scratch, quarantine)
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return nil, err
				}
				if err := os.Rename(source, target); err != nil {
					return nil, err
				}
				conflicted = append(conflicted, Conflict{Path: rel, Quarantine: quarantine})
				quarantines = append(quarantines, quarantine)
				continue // the last valid canonical file stays untouched
			}
		}
		promote = append(promote, rel)
	}
	promote = append(promote, quarantines...)
	for _, rel := range promote {
		dest := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(scratch, rel), dest); err != nil {
			return nil, err
		}
	}
	return conflicted, nil
}

// Composes only the shape we can prove safe: both descendants
// retain the complete base and add bytes at its end. Semantic edits belong in
// quarantine, never in an automatic union.
func appendMerge(ours, base, theirs []byte) ([]byte, bool) {
	if !bytes.HasPrefix(ours, base) || !bytes.HasPrefix(theirs, base) {
		return nil, false
	}
	merged := make([]byte, 0, len(ours)+len(theirs)-len(base))
	merged = append(merged, base...)
	merged = append(merged, ours[len(base):]...)
	merged = append(merged, theirs[len(base):]...)
	return merged, true
}

// Copies one staged file into the scratch tree, bounded by the
// same size cap Validate measured against: the file can have grown since.
func copyStagedFile(root *os.Root, rel, dest string) error {
	in, err := openStaged(root, rel)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, maxFile+1))
	if err != nil {
		return err
	}
	if n > maxFile {
		return fmt.Errorf("staged knowledge file %q exceeds %d bytes -- rejected", rel, maxFile)
	}
	return nil
}

// Opens a path inside the staging tree and proves on the
// descriptor itself that it is an ordinary unaliased file -- nothing an
// earlier stat decided is trusted. O_NONBLOCK so a fifo cannot park the
// caller.
func openStaged(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("staged knowledge file %q is not readable inside staging -- rejected: %w", rel, err)
	}
	info, err := f.Stat()
	switch {
	case err != nil:
	case !info.Mode().IsRegular():
		err = fmt.Errorf("staged knowledge file %q is not a regular file -- rejected", rel)
	default:
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			err = fmt.Errorf("staged knowledge file %q is a hard link -- rejected", rel)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
