//go:build windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

type automationFileLock interface {
	Close() error
}

type windowsAutomationFileLock struct {
	file       *os.File
	overlapped windows.Overlapped
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
	lock := &windowsAutomationFileLock{file: file}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	delay := time.NewTicker(10 * time.Millisecond)
	defer delay.Stop()
	for {
		err = windows.LockFileEx(
			windows.Handle(file.Fd()),
			flags,
			0,
			1,
			0,
			&lock.overlapped,
		)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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

func (l *windowsAutomationFileLock) Close() error {
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
	return secretSafeAutomationLockError(errors.Join(unlockErr, closeErr))
}
