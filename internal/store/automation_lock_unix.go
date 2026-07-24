//go:build !windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type automationFileLock interface {
	Close() error
}

type unixAutomationFileLock struct {
	file *os.File
}

func acquireAutomationFileLock(
	ctx context.Context,
	path string,
	exclusive bool,
) (automationFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, secretSafeAutomationLockError(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, secretSafeAutomationLockError(err)
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	delay := time.NewTicker(10 * time.Millisecond)
	defer delay.Stop()
	for {
		err = unix.Flock(int(file.Fd()), operation)
		if err == nil {
			return &unixAutomationFileLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, secretSafeAutomationLockError(err)
		}
		select {
		case <-ctx.Done():
			closeErr := secretSafeAutomationLockError(file.Close())
			return nil, errors.Join(ctx.Err(), closeErr)
		case <-delay.C:
		}
	}
}

func (l *unixAutomationFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return secretSafeAutomationLockError(errors.Join(unlockErr, closeErr))
}
