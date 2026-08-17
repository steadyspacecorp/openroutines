package filetree

import (
	"io/fs"
	"os"
	"path/filepath"
)

type ModePolicy uint8

const (
	DataFiles ModePolicy = iota
	PreserveExecutables
)

type Options struct {
	Mode ModePolicy
	Skip func(rel string, entry fs.DirEntry) bool
}

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
