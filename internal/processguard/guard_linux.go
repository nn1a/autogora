//go:build linux

package processguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	guardMarker        = "__autogora_internal_linux_process_guard_v1__"
	guardModeFenced    = "fenced"
	guardModeImmediate = "immediate"
	startFenceFD       = 3
	fencedProofFD      = 4
	immediateProofFD   = 3

	testIncompleteLineageEnvironment = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_INCOMPLETE_LINEAGE"
	testCleanupLimitEnvironment      = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_CLEANUP_LIMIT_MS"
)

func init() {
	if len(os.Args) < 2 || os.Args[1] != guardMarker {
		return
	}
	os.Exit(runLinuxGuard(os.Args[2:]))
}

func NewFencedCommand(
	ctx context.Context,
	name string,
	args ...string,
) (*FencedCommand, error) {
	return NewFencedCommandContext(ctx, 0, name, args...)
}

// NewFencedCommandContext starts only the durable Linux guard. The guarded
// target remains behind a one-shot fence until Release, while ctx and maximum
// bound both the eventual target and teardown wait.
func NewFencedCommandContext(
	ctx context.Context,
	maximum time.Duration,
	name string,
	args ...string,
) (*FencedCommand, error) {
	if err := requireLinuxGuardCapability(); err != nil {
		return nil, err
	}
	bounded, cancel := boundedContext(ctx, maximum)
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	proof, err := newLinuxTeardownProof()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		cancel()
		return nil, err
	}
	arguments := guardArguments(guardModeFenced, os.Getpid(), fencedProofFD, name, args)
	command := newLinuxGuardCommandContext(bounded, arguments...)
	command.ExtraFiles = []*os.File{reader, proof.writer}
	return newFencedCommand(
		ctx,
		bounded,
		cancel,
		command,
		reader,
		writer,
		proof,
	), nil
}

func newGuardedCommandContext(
	ctx context.Context,
	name string,
	args ...string,
) (*exec.Cmd, teardownProof) {
	proof, err := newLinuxTeardownProof()
	if err != nil {
		command := exec.CommandContext(ctx, "/proc/self/exe", guardMarker, "invalid")
		return command, failedTeardownProof{err: err}
	}
	arguments := guardArguments(guardModeImmediate, os.Getpid(), immediateProofFD, name, args)
	command := newLinuxGuardCommandContext(ctx, arguments...)
	command.ExtraFiles = []*os.File{proof.writer}
	return command, proof
}

func newLinuxGuardCommandContext(
	ctx context.Context,
	arguments ...string,
) *exec.Cmd {
	command := exec.CommandContext(ctx, "/proc/self/exe", arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
	// CommandContext's default SIGKILL would bypass the guard's descendant
	// teardown. A bounded caller context requests graceful guard shutdown;
	// Wait only returns after the guard proves its descendant set is empty.
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := command.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	return command
}

func IsGuardCommand(command *exec.Cmd) bool {
	return command != nil &&
		len(command.Args) > 1 &&
		command.Args[1] == guardMarker
}

// TeardownProofAvailable reports whether this platform runner can distinguish
// a clean guard exit from an abnormal exit and attest ancestry cleanup.
func TeardownProofAvailable() bool { return true }

func guardArguments(mode string, parentPID, proofFD int, name string, args []string) []string {
	result := []string{
		guardMarker,
		mode,
		strconv.Itoa(parentPID),
		strconv.Itoa(proofFD),
		"--",
		name,
	}
	return append(result, args...)
}

func requireLinuxGuardCapability() error {
	for _, path := range []string{"/proc/self/exe", "/proc/self/stat"} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("linux process guard requires procfs: %w", err)
		}
	}
	return nil
}

type linuxTeardownProof struct {
	reader *os.File
	writer *os.File
	once   sync.Once
	result error
}

func newLinuxTeardownProof() (*linuxTeardownProof, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &linuxTeardownProof{reader: reader, writer: writer}, nil
}

func (p *linuxTeardownProof) afterStart() error {
	return p.writer.Close()
}

func (p *linuxTeardownProof) confirm() error {
	p.once.Do(func() {
		_ = p.writer.Close()
		var value [1]byte
		count, err := io.ReadFull(p.reader, value[:])
		_ = p.reader.Close()
		if count != 1 || err != nil || value[0] != 'Q' {
			p.result = errors.Join(ErrTeardownUnconfirmed, err)
		}
	})
	return p.result
}

func (p *linuxTeardownProof) close() {
	_ = p.writer.Close()
	_ = p.reader.Close()
}

type failedTeardownProof struct {
	err error
}

func (p failedTeardownProof) afterStart() error { return p.err }
func (p failedTeardownProof) confirm() error {
	return errors.Join(ErrTeardownUnconfirmed, p.err)
}
func (p failedTeardownProof) close() {}

type guardInvocation struct {
	mode      string
	parentPID int
	proofFD   int
	command   string
	args      []string
}

func parseGuardInvocation(args []string) (guardInvocation, error) {
	if len(args) < 5 || (args[0] != guardModeFenced && args[0] != guardModeImmediate) ||
		args[3] != "--" || strings.TrimSpace(args[4]) == "" {
		return guardInvocation{}, errors.New("invalid internal process guard invocation")
	}
	parentPID, err := strconv.Atoi(args[1])
	if err != nil || parentPID <= 0 {
		return guardInvocation{}, errors.New("invalid internal process guard parent")
	}
	proofFD, err := strconv.Atoi(args[2])
	if err != nil || proofFD < 3 {
		return guardInvocation{}, errors.New("invalid internal process guard proof descriptor")
	}
	return guardInvocation{
		mode:      args[0],
		parentPID: parentPID,
		proofFD:   proofFD,
		command:   args[4],
		args:      append([]string(nil), args[5:]...),
	}, nil
}

func runLinuxGuard(args []string) int {
	invocation, err := parseGuardInvocation(args)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 125
	}
	proof := os.NewFile(uintptr(invocation.proofFD), "autogora-teardown-proof")
	if proof == nil {
		_, _ = fmt.Fprintln(os.Stderr, "missing teardown proof descriptor")
		return 125
	}
	unix.CloseOnExec(invocation.proofFD)
	defer proof.Close()
	attest := func(code int) int {
		if _, err := proof.Write([]byte{'Q'}); err != nil {
			return 126
		}
		return code
	}
	if err := establishLinuxGuard(invocation.parentPID); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return attest(125)
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)

	if invocation.mode == guardModeFenced {
		released, interrupted := awaitStartFence(signals)
		if !released {
			if interrupted != nil {
				return attest(128 + int(interrupted.(syscall.Signal)))
			}
			return attest(125)
		}
	}
	select {
	case received := <-signals:
		return attest(128 + int(received.(syscall.Signal)))
	default:
	}

	target := exec.Command(invocation.command, invocation.args...)
	target.Stdin = os.Stdin
	target.Stdout = os.Stdout
	target.Stderr = os.Stderr
	target.Env = strippedGuardEnvironment(os.Environ())
	target.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := target.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "start guarded command: %v\n", err)
		return attest(127)
	}

	waited := make(chan error, 1)
	go func() {
		waited <- target.Wait()
	}()

	var (
		targetErr error
		received  os.Signal
	)
	select {
	case targetErr = <-waited:
	case received = <-signals:
		var quiescent bool
		targetErr, quiescent = terminateAndReapDescendants(
			target,
			waited,
			time.Now().Add(linuxGuardCleanupLimit()),
		)
		if !quiescent {
			return 124
		}
	}
	if received == nil {
		if !terminateAndReapAfterMainExit(
			time.Now().Add(linuxGuardCleanupLimit()),
		) {
			// Exiting without the Q byte is an explicit negative
			// attestation. The parent reports ErrTeardownUnconfirmed and
			// moves the durable run to operator intervention.
			return 124
		}
		return attest(guardedExitCode(targetErr))
	}
	return attest(128 + int(received.(syscall.Signal)))
}

func establishLinuxGuard(expectedParent int) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable child subreaper: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0); err != nil {
		return fmt.Errorf("enable parent-death signal: %w", err)
	}
	if os.Getppid() != expectedParent {
		return errors.New("process guard parent exited during startup")
	}
	return nil
}

func awaitStartFence(signals <-chan os.Signal) (bool, os.Signal) {
	fence := os.NewFile(uintptr(startFenceFD), "autogora-start-fence")
	if fence == nil {
		return false, nil
	}
	defer fence.Close()
	result := make(chan bool, 1)
	go func() {
		var value [1]byte
		count, err := fence.Read(value[:])
		result <- count == 1 && err == nil
	}()
	select {
	case released := <-result:
		return released, nil
	case received := <-signals:
		_ = fence.Close()
		return false, received
	}
}

func strippedGuardEnvironment(environment []string) []string {
	// The current guard protocol uses argv rather than environment control
	// fields. Keep this filter so future internal fields cannot leak into a
	// coding agent or Git hook.
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, "AUTOGORA_INTERNAL_PROCESS_GUARD_") {
			continue
		}
		result = append(result, item)
	}
	return result
}

func linuxGuardCleanupLimit() time.Duration {
	const defaultLimit = 5 * time.Second
	value := strings.TrimSpace(os.Getenv(testCleanupLimitEnvironment))
	if value == "" {
		return defaultLimit
	}
	milliseconds, err := strconv.Atoi(value)
	if err != nil || milliseconds < 10 || milliseconds > int(defaultLimit/time.Millisecond) {
		return defaultLimit
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func terminateAndReapDescendants(
	target *exec.Cmd,
	waited <-chan error,
	deadline time.Time,
) (error, bool) {
	_ = target.Process.Signal(syscall.SIGTERM)
	signalDescendants(syscall.SIGTERM)
	graceDeadline := time.Now().Add(750 * time.Millisecond)
	if deadline.Before(graceDeadline) {
		graceDeadline = deadline
	}
	for time.Now().Before(graceDeadline) {
		select {
		case err := <-waited:
			return err, terminateAndReapAfterMainExit(deadline)
		default:
		}
		signalDescendants(syscall.SIGTERM)
		time.Sleep(10 * time.Millisecond)
	}
	_ = target.Process.Kill()
	for {
		if time.Now().After(deadline) {
			return nil, false
		}
		signalDescendants(syscall.SIGKILL)
		select {
		case err := <-waited:
			return err, terminateAndReapAfterMainExit(deadline)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func terminateAndReapAfterMainExit(deadline time.Time) bool {
	graceDeadline := time.Now().Add(250 * time.Millisecond)
	if deadline.Before(graceDeadline) {
		graceDeadline = deadline
	}
	for {
		hasChildren, waitComplete := reapAdoptedChildren()
		if waitComplete && !hasChildren {
			return true
		}
		count, complete := signalDescendants(syscall.SIGTERM)
		if complete && count == 0 && quietDescendantSet(3, 10*time.Millisecond) {
			return true
		}
		if time.Now().After(graceDeadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		if time.Now().After(deadline) {
			return false
		}
		hasChildren, waitComplete := reapAdoptedChildren()
		if waitComplete && !hasChildren {
			return true
		}
		count, complete := signalDescendants(syscall.SIGKILL)
		if complete && count == 0 && quietDescendantSet(3, 10*time.Millisecond) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func quietDescendantSet(rounds int, interval time.Duration) bool {
	for index := 0; index < rounds; index++ {
		hasChildren, waitComplete := reapAdoptedChildren()
		if waitComplete && !hasChildren {
			// Once the guarded main has been reaped, every surviving
			// descendant tree has a live root child of this subreaper.
			// ECHILD is therefore an atomic zero-descendant proof.
			return true
		}
		if !waitComplete {
			return false
		}
		count, complete := descendantCount()
		if !complete || count != 0 {
			return false
		}
		if index+1 < rounds {
			time.Sleep(interval)
		}
	}
	return true
}

type processHandle struct {
	pid int
	fd  int
}

func signalDescendants(signal syscall.Signal) (int, bool) {
	handles, complete := descendantHandles()
	defer func() {
		for _, handle := range handles {
			_ = unix.Close(handle.fd)
		}
	}()
	for _, handle := range handles {
		err := unix.PidfdSendSignal(handle.fd, signal, nil, 0)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			complete = false
		}
	}
	return len(handles), complete
}

func descendantCount() (int, bool) {
	handles, complete := descendantHandles()
	for _, handle := range handles {
		_ = unix.Close(handle.fd)
	}
	return len(handles), complete
}

func descendantHandles() ([]processHandle, bool) {
	if os.Getenv(testIncompleteLineageEnvironment) == "1" {
		return nil, false
	}
	self := os.Getpid()
	queue := []int{self}
	owned := map[int]bool{self: true}
	handles := make(map[int]processHandle)
	complete := true
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		children, childrenComplete := linuxProcessChildren(parent)
		if !childrenComplete {
			complete = false
		}
		for _, pid := range children {
			if pid <= 0 || owned[pid] {
				continue
			}
			expected, statErr := linuxProcessState(pid)
			if statErr != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					complete = false
				}
				continue
			}
			if !owned[expected.parentPID] {
				// The children file and stat view crossed a reparent or PID
				// reuse boundary. A fresh traversal will either find it under
				// the subreaper or prove that it is unrelated.
				complete = false
				continue
			}
			fd, openErr := unix.PidfdOpen(pid, 0)
			if errors.Is(openErr, unix.ESRCH) || errors.Is(openErr, os.ErrNotExist) {
				continue
			}
			if openErr != nil {
				complete = false
				continue
			}
			observed, observedErr := linuxProcessState(pid)
			if observedErr != nil || observed != expected {
				_ = unix.Close(fd)
				if observedErr != nil && errors.Is(observedErr, os.ErrNotExist) {
					continue
				}
				complete = false
				continue
			}
			owned[pid] = true
			handles[pid] = processHandle{pid: pid, fd: fd}
			queue = append(queue, pid)
		}
	}
	result := make([]processHandle, 0, len(handles))
	for _, handle := range handles {
		result = append(result, handle)
	}
	return result, complete
}

func linuxProcessChildren(pid int) ([]int, bool) {
	taskDirectory := filepath.Join("/proc", strconv.Itoa(pid), "task")
	threads, err := os.ReadDir(taskDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	children := make(map[int]bool)
	complete := true
	for _, thread := range threads {
		if _, err := strconv.Atoi(thread.Name()); err != nil {
			continue
		}
		value, readErr := os.ReadFile(filepath.Join(taskDirectory, thread.Name(), "children"))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			complete = false
			continue
		}
		for _, field := range strings.Fields(string(value)) {
			child, parseErr := strconv.Atoi(field)
			if parseErr != nil || child <= 0 {
				complete = false
				continue
			}
			children[child] = true
		}
	}
	result := make([]int, 0, len(children))
	for child := range children {
		result = append(result, child)
	}
	return result, complete
}

type linuxProcessStat struct {
	parentPID int
	startTime uint64
}

func linuxProcessState(pid int) (linuxProcessStat, error) {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return linuxProcessStat{}, err
	}
	closing := strings.LastIndexByte(string(contents), ')')
	if closing < 0 {
		return linuxProcessStat{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(contents[closing+1:]))
	if len(fields) <= 19 {
		return linuxProcessStat{}, errors.New("incomplete process stat")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return linuxProcessStat{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return linuxProcessStat{}, err
	}
	return linuxProcessStat{parentPID: parentPID, startTime: startTime}, nil
}

func reapAdoptedChildren() (bool, bool) {
	if os.Getenv(testIncompleteLineageEnvironment) == "1" {
		return true, false
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return false, true
		}
		if err != nil {
			return false, false
		}
		if pid == 0 {
			return true, true
		}
	}
}

func guardedExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
		return exitError.ExitCode()
	}
	if errors.Is(err, io.EOF) {
		return 0
	}
	return 127
}
