//go:build linux

package processguard

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func withExactFencedProcess(
	identity DurableIdentity,
	signals ...unix.Signal,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	bootID, err := linuxBootID()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrExactProcessSignalUnavailable, err)
	}
	if bootID != identity.BootID {
		return ErrDurableProcessIdentityChanged
	}
	namespace, err := linuxProcessPIDNamespace(os.Getpid())
	if err != nil {
		return fmt.Errorf("%w: inspect current PID namespace: %v",
			ErrExactProcessSignalUnavailable, err)
	}
	if namespace.device != identity.PIDNamespaceDevice ||
		namespace.inode != identity.PIDNamespaceInode {
		return ErrDurableProcessIdentityChanged
	}

	pidfd, err := unix.PidfdOpen(identity.GuardPID, 0)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist) {
		return os.ErrProcessDone
	}
	if err != nil {
		return fmt.Errorf("%w: open guard pidfd: %v",
			ErrExactProcessSignalUnavailable, err)
	}
	defer unix.Close(pidfd)

	state, err := linuxProcessState(identity.GuardPID)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrProcessDone
	}
	if err != nil {
		return fmt.Errorf("%w: inspect guard process state: %v",
			ErrExactProcessSignalUnavailable, err)
	}
	if state.state == 'Z' {
		return os.ErrProcessDone
	}
	if state.startTime != identity.StartTimeTicks ||
		state.processGroupID != identity.ProcessGroupID {
		return ErrDurableProcessIdentityChanged
	}
	for index, signal := range signals {
		if err := unix.PidfdSendSignal(pidfd, signal, nil, 0); errors.Is(
			err,
			unix.ESRCH,
		) {
			if index == 0 {
				return os.ErrProcessDone
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("%w: send signal through guard pidfd: %v",
				ErrExactProcessSignalUnavailable, err)
		}
	}
	return nil
}

func verifyExactFencedProcessSignal(identity DurableIdentity) error {
	return withExactFencedProcess(identity, 0)
}

func requestExactFencedProcessStop(identity DurableIdentity) error {
	return withExactFencedProcess(
		identity,
		unix.SIGTERM,
		unix.SIGCONT,
	)
}
