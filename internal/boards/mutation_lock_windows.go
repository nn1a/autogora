//go:build windows

package boards

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type boardMutationLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireBoardMutationLock(path string, exclusive bool) (*boardMutationLock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	lock := &boardMutationLock{file: file}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		flags,
		0,
		1,
		0,
		&lock.overlapped,
	)
	if err != nil {
		closeErr := file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, closeErr
		}
		return nil, false, errors.Join(err, closeErr)
	}
	return lock, true, nil
}

func (l *boardMutationLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(
		windows.Handle(l.file.Fd()),
		0,
		1,
		0,
		&l.overlapped,
	)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
