//go:build !windows

package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nn1a/autogora/internal/processguard"
)

// preflightWorkerCommand rejects deterministic exec failures before the
// durable start gate reserves anything. The path is made absolute so changing
// into the worker workspace cannot alter which executable the shell invokes.
func preflightWorkerCommand(command RunnerCommand) (RunnerCommand, error) {
	path := command.Command
	if strings.ContainsRune(path, filepath.Separator) && !filepath.IsAbs(path) && command.CWD != "" {
		path = filepath.Join(command.CWD, path)
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return command, err
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return command, err
		}
	}
	if err := validateUnixExecutable(resolved, command.CWD); err != nil {
		return command, err
	}
	command.Command = resolved
	return command, nil
}

func validateUnixExecutable(path, cwd string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var prefix [256]byte
	count, readErr := file.Read(prefix[:])
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	value := prefix[:count]
	if len(value) >= 4 && supportedUnixBinaryMagic(value[:4]) {
		return nil
	}
	if len(value) >= 2 && value[0] == '#' && value[1] == '!' {
		line := value[2:]
		if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
			line = line[:newline]
		}
		fields := strings.Fields(string(line))
		if len(fields) == 0 {
			return &os.PathError{Op: "fork/exec", Path: path, Err: syscall.ENOEXEC}
		}
		interpreter := fields[0]
		if !filepath.IsAbs(interpreter) {
			if cwd == "" {
				cwd = "."
			}
			interpreter = filepath.Join(cwd, interpreter)
		}
		if _, err := exec.LookPath(interpreter); err != nil {
			return &os.PathError{Op: "fork/exec", Path: path, Err: err}
		}
		return nil
	}
	return &os.PathError{Op: "fork/exec", Path: path, Err: syscall.ENOEXEC}
}

func supportedUnixBinaryMagic(value []byte) bool {
	if len(value) < 4 {
		return false
	}
	magic := uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
	switch magic {
	case 0x7f454c46, // ELF
		0xfeedface, 0xcefaedfe, // Mach-O 32-bit
		0xfeedfacf, 0xcffaedfe, // Mach-O 64-bit
		0xcafebabe, 0xbebafeca, // universal Mach-O
		0xcafebabf, 0xbfbafeca: // universal Mach-O 64-bit
		return true
	default:
		return false
	}
}

// newWorkerCommand starts a platform guard behind a pipe barrier. On Linux the
// guard remains the durable process for the whole turn, becomes a child
// subreaper, and does not start the coding agent until the dispatcher records
// the guard PID and identity.
func newWorkerCommand(ctx context.Context, command RunnerCommand) (*workerCommand, error) {
	guarded, err := processguard.NewFencedCommand(ctx, command.Command, command.Args...)
	if err != nil {
		return nil, err
	}
	return &workerCommand{
		child:   guarded.Command,
		start:   guarded.Start,
		wait:    guarded.Wait,
		release: guarded.Release,
		cleanup: guarded.Close,
	}, nil
}

func configureProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func attachProcessTree(cmd *exec.Cmd) (func(), error) {
	pid := cmd.Process.Pid
	return func() {
		// This closure runs only for the exact command started by this
		// dispatcher. Unlike restart recovery, the group cannot have been
		// confused with an unrelated persisted PID, so it is safe to clean up
		// background descendants before allowing the run to become terminal.
		if !processGroupAlive(pid) {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		if waitForProcessGroup(pid, time.Second) {
			return
		}
		if processguard.IsGuardCommand(cmd) {
			// The Linux guard escalates its descendants itself. Killing the
			// subreaper would destroy teardown proof and let setsid children
			// escape. Leave a non-quiescent guard for recovery/operator action.
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = waitForProcessGroup(pid, time.Second)
	}, nil
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitForProcessGroup(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return !processGroupAlive(pid)
}

func terminateProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force && !processguard.IsGuardCommand(cmd) {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
