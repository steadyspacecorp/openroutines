package runner

import (
	"io/fs"
	"os"
	"path/filepath"
)

// prepareAttemptOwnership gives the model identity only the trees it may
// mutate. The workspace and pristine base remain owned by the supervisor;
// ordinary read and traversal bits let the attempt consume their contents.
func prepareAttemptOwnership(uid uint32, roots ...string) error {
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := os.Chown(path, int(uid), int(uid)); err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return os.Chmod(path, 0o600)
		}); err != nil {
			return err
		}
	}
	return nil
}

func prepareWorkspaceAccess(workspace string) error {
	return filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	})
}
