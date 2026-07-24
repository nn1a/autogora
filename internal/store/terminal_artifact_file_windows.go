//go:build windows

package store

import "golang.org/x/sys/windows"

func replaceTerminalArtifactFile(staging, target string) error {
	from, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		from,
		to,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

// Windows does not expose a portable directory fsync. MoveFileEx with
// MOVEFILE_WRITE_THROUGH is the durability boundary for the replacement;
// directory creation itself is journaled by supported Windows filesystems.
func syncTerminalArtifactDirectory(string) error {
	return nil
}
