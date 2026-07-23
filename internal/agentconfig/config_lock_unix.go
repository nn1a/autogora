//go:build !windows

package agentconfig

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type configLock struct {
	file *os.File
}

func acquireConfigLock(path string) (*configLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &configLock{file: file}, nil
}

func (l *configLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
