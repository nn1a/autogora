//go:build windows

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newWorkerCommand(_ context.Context, command RunnerCommand) (*workerCommand, error) {
	child := exec.Command(command.Command, command.Args...)
	return &workerCommand{
		child: child,
		start: child.Start,
		wait:  child.Wait,
		release: func() (bool, error) {
			if child.Process == nil {
				return false, errors.New("worker process has not started")
			}
			if err := resumeSuspendedProcess(uint32(child.Process.Pid)); err != nil {
				return false, err
			}
			return true, nil
		},
		cleanup: func() {},
	}, nil
}

func preflightWorkerCommand(command RunnerCommand) (RunnerCommand, error) {
	return command, nil
}

func configureProcess(cmd *exec.Cmd) {
	// The worker must not execute before it belongs to the Job Object and its
	// durable spawn record exists. release on workerCommand resumes it.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}

// attachProcessTree places each dispatcher-owned worker in a kill-on-close Job
// Object. The handle remains open until Wait returns, and the operating system
// closes it if the supervisor exits, so worker descendants cannot outlive the
// dispatcher silently.
func attachProcessTree(cmd *exec.Cmd) (func(), error) {
	if cmd.Process == nil {
		return nil, fmt.Errorf("worker process has not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create worker job object: %w", err)
	}
	closeJob := func() {
		_ = windows.CloseHandle(job)
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		closeJob()
		return nil, fmt.Errorf("configure worker job object: %w", err)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		closeJob()
		return nil, fmt.Errorf("open worker for job assignment: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		closeJob()
		return nil, fmt.Errorf("assign worker job object: %w", err)
	}
	return closeJob, nil
}

func resumeSuspendedProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return resumeErr
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	return fmt.Errorf("primary thread not found for PID %d", pid)
}

func terminateProcess(cmd *exec.Cmd, _ bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
