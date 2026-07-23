//go:build !windows

package filesecurity

import (
	"errors"
	"os"
)

// RestrictToCurrentUser applies the Unix permission contract used for
// host-authored handoff files.
func RestrictToCurrentUser(path string) error {
	return os.Chmod(path, 0o600)
}

// RestrictDirectoryToCurrentUser applies the Unix private-directory contract
// for security-sensitive handoff parents.
func RestrictDirectoryToCurrentUser(path string) error {
	return os.Chmod(path, 0o700)
}

// ValidateCurrentUserFile rejects links, special files, and any permission
// bits beyond owner read/write.
func ValidateCurrentUserFile(_ string, info os.FileInfo) error {
	if info == nil {
		return errors.New("file metadata is missing")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("file must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("file is not regular")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("file permissions must be 0600")
	}
	return nil
}
