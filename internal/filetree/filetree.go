// Package filetree copies trusted directory trees with an explicit mode policy.
package filetree

import (
	"io/fs"
	"os"
	"path/filepath"
)

// ModePolicy chooses the permissions assigned to copied regular files.
type ModePolicy uint8

const (
	// DataFiles writes every regular file as non-executable.
	DataFiles ModePolicy = iota
	// PreserveExecutables carries the source executable bit into the copy.
	PreserveExecutables
)

// Options controls filtering and regular-file permissions.
type Options struct {
	Mode ModePolicy
	Skip func(rel string, entry fs.DirEntry) bool
}

// CopyRegular copies directories and regular files, ignoring special files.
func CopyRegular(src, dst string, options Options) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		if options.Skip != nil && options.Skip(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if options.Mode == PreserveExecutables {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, raw, mode); err != nil {
			return err
		}
		return os.Chmod(target, mode)
	})
}
