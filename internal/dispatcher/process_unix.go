//go:build !windows

package dispatcher

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

// newWorkerCommand starts a tiny shell barrier in the worker's eventual
// process slot. The shell cannot exec the coding agent until the dispatcher
// records this exact PID and identity. If the dispatcher dies first, its pipe
// closes and the barrier exits without running the agent.
func newWorkerCommand(command RunnerCommand) (*workerCommand, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-c",
		`IFS= read -r autogora_start <&3 || exit 125; exec "$@"`,
		"autogora-start-barrier",
		command.Command,
	}
	args = append(args, command.Args...)
	child := exec.Command("/bin/sh", args...)
	child.ExtraFiles = []*os.File{reader}
	cleanup := func() {
		_ = reader.Close()
		_ = writer.Close()
	}
	return &workerCommand{
		child: child,
		release: func() (bool, error) {
			written, writeErr := writer.Write([]byte{'\n'})
			closeWriterErr := writer.Close()
			closeReaderErr := reader.Close()
			if written == 1 {
				return true, nil
			}
			return false, errors.Join(writeErr, closeWriterErr, closeReaderErr, io.ErrShortWrite)
		},
		cleanup: cleanup,
	}, nil
}

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
