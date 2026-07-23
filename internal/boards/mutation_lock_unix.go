//go:build !windows

package boards

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type boardMutationLock struct {
	file *os.File
}

func acquireBoardMutationLock(path string, exclusive bool) (*boardMutationLock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		closeErr := file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, closeErr
		}
		return nil, false, errors.Join(err, closeErr)
	}
	return &boardMutationLock{file: file}, true, nil
}

func (l *boardMutationLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
