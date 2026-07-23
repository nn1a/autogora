//go:build windows

package agentconfig

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type configLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireConfigLock(path string) (*configLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &configLock{file: file}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&lock.overlapped,
	)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return lock, nil
}

func (l *configLock) Close() error {
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
