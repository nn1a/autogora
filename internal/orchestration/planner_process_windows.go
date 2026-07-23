//go:build windows

package orchestration

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configurePlannerProcess(cmd *exec.Cmd) {
	// Start suspended so the planner cannot create descendants before it is
	// assigned to the kill-on-close Job Object.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}

func attachPlannerProcessTree(cmd *exec.Cmd) (func(), error) {
	if cmd.Process == nil {
		return nil, fmt.Errorf("planner process has not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create planner job object: %w", err)
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
		return nil, fmt.Errorf("configure planner job object: %w", err)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		closeJob()
		return nil, fmt.Errorf("open planner for job assignment: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		closeJob()
		return nil, fmt.Errorf("assign planner job object: %w", err)
	}
	if err := resumeSuspendedPlannerProcess(uint32(cmd.Process.Pid)); err != nil {
		closeJob()
		return nil, fmt.Errorf("resume protected planner: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = windows.TerminateJobObject(job, 1)
			closeJob()
		})
	}, nil
}

func resumeSuspendedPlannerProcess(pid uint32) error {
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
		thread, openErr := windows.OpenThread(
			windows.THREAD_SUSPEND_RESUME,
			false,
			entry.ThreadID,
		)
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
