//go:build linux

package processguard

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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
	guardModeDurable   = "fenced-durable"
	guardModeImmediate = "immediate"
	startFenceFD       = 3
	fencedProofFD      = 4
	readyIdentityFD    = 5
	durableReceiptFD   = 6
	immediateProofFD   = 3

	testIncompleteLineageEnvironment = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_INCOMPLETE_LINEAGE"
	testCleanupLimitEnvironment      = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_CLEANUP_LIMIT_MS"
	testStopBeforeProofReady         = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_STOP_BEFORE_PROOF_READY"
	testStopBeforeReadyEnvironment   = "AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_STOP_BEFORE_READY"
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
	return newLinuxFencedCommandContext(
		ctx,
		maximum,
		nil,
		name,
		args...,
	)
}

// NewFencedCommandContextWithDurableReceipt starts a fenced Linux guard whose
// positive descendant-teardown attestation is also written and synced to the
// caller's pre-opened receipt file. The caller retains ownership of
// receipt.File; the command owns an internal duplicate.
func NewFencedCommandContextWithDurableReceipt(
	ctx context.Context,
	maximum time.Duration,
	receipt DurableReceiptConfig,
	name string,
	args ...string,
) (*FencedCommand, error) {
	return newLinuxFencedCommandContext(
		ctx,
		maximum,
		&receipt,
		name,
		args...,
	)
}

func newLinuxFencedCommandContext(
	ctx context.Context,
	maximum time.Duration,
	receipt *DurableReceiptConfig,
	name string,
	args ...string,
) (*FencedCommand, error) {
	var (
		durableLease  *linuxDurableReceiptLease
		durableFile   *os.File
		receiptSource *os.File
		executionID   string
		receiptID     string
		err           error
	)
	if receipt != nil {
		// Consume the one-shot config before every other setup step. Any
		// validation, capability, descriptor, or pipe failure permanently
		// prevents this config from being reused.
		durableLease, err = acquireLinuxDurableReceipt(*receipt)
		if err != nil {
			return nil, err
		}
		durableFile = durableLease.file
		executionID = receipt.ExecutionID
		receiptID = receipt.ReceiptID
		receiptSource = receipt.File
	} else {
		executionID, err = NewDurableIdentifier()
		if err != nil {
			return nil, err
		}
		receiptID, err = NewDurableIdentifier()
		if err != nil {
			return nil, err
		}
	}
	if err := requireLinuxGuardCapability(); err != nil {
		if durableLease != nil {
			err = errors.Join(err, durableLease.close())
		}
		return nil, err
	}
	bounded, boundedCancel := boundedContext(ctx, maximum)
	launchContext, launchCancel := context.WithCancel(context.Background())
	cancel := func() {
		boundedCancel()
		launchCancel()
	}
	fenceParent, fenceGuard, err := newLinuxGuardSocketPair(
		"autogora-start-fence-parent",
		"autogora-start-fence-guard",
	)
	if err != nil {
		if durableLease != nil {
			err = errors.Join(err, durableLease.close())
		}
		cancel()
		return nil, err
	}
	proof, err := newLinuxTeardownProof()
	if err != nil {
		_ = fenceParent.Close()
		_ = fenceGuard.Close()
		if durableLease != nil {
			err = errors.Join(err, durableLease.close())
		}
		cancel()
		return nil, err
	}
	identityReady, readyGuard, err := newLinuxIdentityHandshake(
		executionID,
		receiptID,
	)
	if err != nil {
		_ = fenceParent.Close()
		_ = fenceGuard.Close()
		proof.close()
		if durableLease != nil {
			err = errors.Join(err, durableLease.close())
		}
		cancel()
		return nil, err
	}
	arguments := fencedGuardArguments(
		os.Getpid(),
		fencedProofFD,
		readyIdentityFD,
		executionID,
		receiptID,
		name,
		args,
	)
	if durableFile != nil {
		arguments = durableGuardArguments(
			os.Getpid(),
			fencedProofFD,
			readyIdentityFD,
			durableReceiptFD,
			executionID,
			receiptID,
			name,
			args,
		)
	}
	command := newLinuxGuardCommandContext(launchContext, arguments...)
	command.ExtraFiles = []*os.File{fenceGuard, proof.writer, readyGuard}
	if durableFile != nil {
		command.ExtraFiles = append(command.ExtraFiles, durableFile)
	}
	fenced := newFencedCommand(
		ctx,
		bounded,
		cancel,
		command,
		fenceGuard,
		fenceParent,
		proof,
	)
	fenced.identityReady = identityReady
	fenced.startFiles = []*os.File{readyGuard}
	fenced.lifetimeLease = durableLease
	fenced.startValidator = newLinuxFencedStartValidator(
		command,
		command.ExtraFiles,
		readyGuard,
		durableFile,
		receiptSource,
	)
	return fenced, nil
}

func newGuardedCommandContext(
	ctx context.Context,
	name string,
	args ...string,
) (*exec.Cmd, teardownProof, error) {
	if err := requireLinuxGuardCapability(); err != nil {
		command := exec.CommandContext(
			ctx,
			"/proc/self/exe",
			guardMarker,
			"invalid",
		)
		return command, failedTeardownProof{err: err}, err
	}
	proof, err := newLinuxTeardownProof()
	if err != nil {
		command := exec.CommandContext(ctx, "/proc/self/exe", guardMarker, "invalid")
		return command, failedTeardownProof{err: err}, err
	}
	arguments := guardArguments(guardModeImmediate, os.Getpid(), immediateProofFD, name, args)
	command := newLinuxGuardCommandContext(ctx, arguments...)
	command.ExtraFiles = []*os.File{proof.writer}
	return command, proof, nil
}

func newLinuxGuardCommandContext(
	ctx context.Context,
	arguments ...string,
) *exec.Cmd {
	command := exec.CommandContext(ctx, "/proc/self/exe", arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGCONT,
	}
	// CommandContext's default SIGKILL would bypass the guard's descendant
	// teardown. A bounded caller context requests graceful guard shutdown;
	// Wait only returns after the guard proves its descendant set is empty.
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		termErr := command.Process.Signal(syscall.SIGTERM)
		if errors.Is(termErr, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		// SIGTERM remains pending while an untrusted target has stopped its
		// parent guard. Resume the exact unreaped os.Process so the trusted
		// signal handler can attest descendant cleanup.
		continueErr := command.Process.Signal(syscall.SIGCONT)
		if errors.Is(continueErr, os.ErrProcessDone) {
			continueErr = nil
		}
		return errors.Join(termErr, continueErr)
	}
	// If a guard cannot run its signal handler (for example, it is stopped
	// before readiness), os/exec eventually reaps it instead of allowing Start
	// or Wait to block forever. Normal guard cleanup has the earlier deadline.
	command.WaitDelay = teardownConfirmationLimit
	return command
}

func IsGuardCommand(command *exec.Cmd) bool {
	return command != nil &&
		len(command.Args) > 1 &&
		command.Args[1] == guardMarker
}

// TeardownProofAvailable reports whether this process can use the Linux guard
// without passing ptrace authority through to an untrusted target.
func TeardownProofAvailable() bool {
	return requireLinuxGuardCapability() == nil
}

func teardownProofUnavailableReason() string {
	if err := requireLinuxGuardCapability(); err != nil {
		return err.Error()
	}
	return ""
}

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

func fencedGuardArguments(
	parentPID int,
	proofFD int,
	readyFD int,
	executionID string,
	receiptID string,
	name string,
	args []string,
) []string {
	result := []string{
		guardMarker,
		guardModeFenced,
		strconv.Itoa(parentPID),
		strconv.Itoa(proofFD),
		strconv.Itoa(readyFD),
		executionID,
		receiptID,
		"--",
		name,
	}
	return append(result, args...)
}

func durableGuardArguments(
	parentPID int,
	proofFD int,
	readyFD int,
	receiptFD int,
	executionID string,
	receiptID string,
	name string,
	args []string,
) []string {
	result := []string{
		guardMarker,
		guardModeDurable,
		strconv.Itoa(parentPID),
		strconv.Itoa(proofFD),
		strconv.Itoa(readyFD),
		strconv.Itoa(receiptFD),
		executionID,
		receiptID,
		"--",
		name,
	}
	return append(result, args...)
}

func newLinuxFencedStartValidator(
	command *exec.Cmd,
	extraFiles []*os.File,
	readyFile *os.File,
	durableFile *os.File,
	receiptSource *os.File,
) func() error {
	expectedPath := command.Path
	expectedArgs := append([]string(nil), command.Args...)
	expectedFiles := append([]*os.File(nil), extraFiles...)
	expectedFileInfo := make([]os.FileInfo, len(expectedFiles))
	expectedFileFlags := make([]int, len(expectedFiles))
	expectedDescriptorFlags := make([]int, len(expectedFiles))
	var snapshotErr error
	for index, file := range expectedFiles {
		info, err := file.Stat()
		if err != nil {
			snapshotErr = errors.Join(snapshotErr, err)
		} else {
			expectedFileInfo[index] = info
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil {
			snapshotErr = errors.Join(snapshotErr, err)
		} else {
			expectedFileFlags[index] = flags
		}
		descriptorFlags, err := unix.FcntlInt(
			file.Fd(),
			unix.F_GETFD,
			0,
		)
		if err != nil {
			snapshotErr = errors.Join(snapshotErr, err)
		} else {
			expectedDescriptorFlags[index] = descriptorFlags
			if descriptorFlags&unix.FD_CLOEXEC == 0 {
				snapshotErr = errors.Join(
					snapshotErr,
					fmt.Errorf(
						"internal guard descriptor %d lacks close-on-exec",
						index+3,
					),
				)
			}
		}
	}
	var receiptSourceInfo os.FileInfo
	if receiptSource != nil {
		info, err := receiptSource.Stat()
		if err != nil {
			snapshotErr = errors.Join(snapshotErr, err)
		} else {
			receiptSourceInfo = info
			if err := validateDurableReceiptSink(receiptSource); err != nil {
				snapshotErr = errors.Join(snapshotErr, err)
			}
		}
	}
	expectedSysProcAttr := *command.SysProcAttr
	expectedCancel := command.Cancel
	expectedCancelCode := reflect.ValueOf(expectedCancel).Pointer()
	expectedWaitDelay := command.WaitDelay

	return func() error {
		if snapshotErr != nil {
			return fmt.Errorf(
				"snapshot internal guard descriptors: %w",
				snapshotErr,
			)
		}
		if command.Path != expectedPath ||
			!slices.Equal(command.Args, expectedArgs) {
			return errors.New("internal guard executable or arguments changed")
		}
		if command.SysProcAttr == nil ||
			!reflect.DeepEqual(*command.SysProcAttr, expectedSysProcAttr) {
			return errors.New("internal guard process attributes changed")
		}
		if command.Cancel == nil ||
			reflect.ValueOf(command.Cancel).Pointer() != expectedCancelCode ||
			command.WaitDelay != expectedWaitDelay {
			return errors.New("internal guard cancellation policy changed")
		}
		// Function values do not expose closure identity. Restore the captured
		// cancellation closure after the code-pointer check so another guard's
		// same-code closure cannot redirect cancellation.
		command.Cancel = expectedCancel
		if receiptSource != nil {
			info, err := receiptSource.Stat()
			if errors.Is(err, os.ErrClosed) {
				// The caller owns the original descriptor and may close it.
				// The atomic internal duplicate remains locked and sealed.
			} else if err != nil {
				return fmt.Errorf(
					"inspect caller-owned durable receipt descriptor: %w",
					err,
				)
			} else {
				if receiptSourceInfo == nil ||
					!os.SameFile(receiptSourceInfo, info) {
					return errors.New(
						"caller-owned durable receipt descriptor identity changed",
					)
				}
				if err := validateDurableReceiptSink(receiptSource); err != nil {
					return err
				}
			}
		}
		if len(command.ExtraFiles) != len(expectedFiles) {
			return errors.New("internal guard descriptor layout changed")
		}
		for index, expected := range expectedFiles {
			if command.ExtraFiles[index] != expected {
				return fmt.Errorf(
					"internal guard descriptor %d changed",
					index+3,
				)
			}
			info, err := expected.Stat()
			if err != nil {
				return fmt.Errorf(
					"inspect internal guard descriptor %d: %w",
					index+3,
					err,
				)
			}
			if expectedFileInfo[index] == nil ||
				!os.SameFile(expectedFileInfo[index], info) {
				return fmt.Errorf(
					"internal guard descriptor %d identity changed",
					index+3,
				)
			}
			flags, err := unix.FcntlInt(expected.Fd(), unix.F_GETFL, 0)
			if err != nil || flags != expectedFileFlags[index] {
				return fmt.Errorf(
					"internal guard descriptor %d flags changed",
					index+3,
				)
			}
			descriptorFlags, err := unix.FcntlInt(
				expected.Fd(),
				unix.F_GETFD,
				0,
			)
			if err != nil ||
				descriptorFlags != expectedDescriptorFlags[index] ||
				descriptorFlags&unix.FD_CLOEXEC == 0 {
				return fmt.Errorf(
					"internal guard descriptor %d close-on-exec flags changed",
					index+3,
				)
			}
			if expected == durableFile {
				if err := validateDurableReceiptSink(expected); err != nil {
					return err
				}
				continue
			}
			if expected == readyFile {
				if info.Mode()&os.ModeSocket == 0 {
					return fmt.Errorf(
						"internal guard descriptor %d is not a socket",
						index+3,
					)
				}
				continue
			}
			if info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf(
					"internal guard descriptor %d is not a socket",
					index+3,
				)
			}
		}
		return nil
	}
}

func requireLinuxGuardCapability() error {
	for _, path := range []string{"/proc/self/exe", "/proc/self/stat"} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("linux process guard requires procfs: %w", err)
		}
	}
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf(
			"%w: open self pidfd: %v",
			ErrExactProcessSignalUnavailable,
			err,
		)
	}
	defer unix.Close(pidfd)
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return fmt.Errorf(
			"%w: verify pidfd signal support: %v",
			ErrExactProcessSignalUnavailable,
			err,
		)
	}
	header := unix.CapUserHeader{
		Version: unix.LINUX_CAPABILITY_VERSION_3,
	}
	capabilities := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		return fmt.Errorf(
			"inspect Linux process guard capabilities: %w",
			err,
		)
	}
	index := uint(unix.CAP_SYS_PTRACE) / 32
	mask := uint32(1) << (uint(unix.CAP_SYS_PTRACE) % 32)
	available := capabilities[index].Effective |
		capabilities[index].Permitted |
		capabilities[index].Inheritable
	if available&mask != 0 {
		return fmt.Errorf(
			"%w: CAP_SYS_PTRACE must be removed from effective, permitted, and inheritable sets before starting coding agents",
			ErrUnsafeProcessGuardPrivileges,
		)
	}
	return nil
}

type linuxTeardownProof struct {
	reader    *os.File
	writer    *os.File
	once      sync.Once
	mu        sync.Mutex
	ready     bool
	quiescent bool
	result    error
}

func newLinuxTeardownProof() (*linuxTeardownProof, error) {
	reader, writer, err := newLinuxGuardSocketPair(
		"autogora-teardown-proof-parent",
		"autogora-teardown-proof-guard",
	)
	if err != nil {
		return nil, err
	}
	return &linuxTeardownProof{reader: reader, writer: writer}, nil
}

func newLinuxGuardSocketPair(
	parentName string,
	guardName string,
) (*os.File, *os.File, error) {
	pair, err := unix.Socketpair(
		unix.AF_UNIX,
		unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), parentName)
	guard := os.NewFile(uintptr(pair[1]), guardName)
	if parent == nil || guard == nil {
		if parent != nil {
			_ = parent.Close()
		} else {
			_ = unix.Close(pair[0])
		}
		if guard != nil {
			_ = guard.Close()
		} else {
			_ = unix.Close(pair[1])
		}
		return nil, nil, errors.New(
			"create internal process guard socket handles",
		)
	}
	return parent, guard, nil
}

func (p *linuxTeardownProof) afterStart() error {
	closeErr := p.writer.Close()
	ready := []unix.PollFd{{
		Fd:     int32(p.reader.Fd()),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}
	pollCount, pollErr := unix.Poll(
		ready,
		int(linuxReadyHandshakeLimit/time.Millisecond),
	)
	if pollErr != nil {
		return errors.Join(closeErr, pollErr)
	}
	if pollCount != 1 || ready[0].Revents&unix.POLLIN == 0 {
		return errors.Join(
			closeErr,
			errors.New("process guard readiness handshake timed out"),
		)
	}
	var value [1]byte
	count, readErr := io.ReadFull(p.reader, value[:])
	if count != 1 || readErr != nil {
		return errors.Join(
			closeErr,
			fmt.Errorf("process guard readiness handshake: %w", readErr),
		)
	}
	p.mu.Lock()
	switch value[0] {
	case 'R':
		p.ready = true
	case 'Q':
		p.quiescent = true
	default:
		p.mu.Unlock()
		return errors.Join(
			closeErr,
			errors.New("invalid process guard readiness attestation"),
		)
	}
	quiescent := p.quiescent
	p.mu.Unlock()
	if quiescent {
		return errors.Join(
			closeErr,
			errors.New("process guard exited before readiness"),
		)
	}
	return closeErr
}

func (p *linuxTeardownProof) confirm() error {
	p.once.Do(func() {
		_ = p.writer.Close()
		p.mu.Lock()
		quiescent := p.quiescent
		p.mu.Unlock()
		for !quiescent {
			var value [1]byte
			count, err := io.ReadFull(p.reader, value[:])
			if count != 1 || err != nil {
				p.result = errors.Join(ErrTeardownUnconfirmed, err)
				break
			}
			switch value[0] {
			case 'R':
				continue
			case 'Q':
				quiescent = true
			default:
				p.result = errors.Join(
					ErrTeardownUnconfirmed,
					errors.New("invalid process guard teardown attestation"),
				)
			}
			if p.result != nil {
				break
			}
		}
		_ = p.reader.Close()
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
	mode        string
	parentPID   int
	proofFD     int
	readyFD     int
	receiptFD   int
	executionID string
	receiptID   string
	command     string
	args        []string
}

func parseGuardInvocation(args []string) (guardInvocation, error) {
	if len(args) < 5 {
		return guardInvocation{}, errors.New("invalid internal process guard invocation")
	}
	mode := args[0]
	parentPID, err := strconv.Atoi(args[1])
	if err != nil || parentPID <= 0 {
		return guardInvocation{}, errors.New("invalid internal process guard parent")
	}
	proofFD, err := strconv.Atoi(args[2])
	if err != nil || proofFD < 3 {
		return guardInvocation{}, errors.New("invalid internal process guard proof descriptor")
	}
	if mode == guardModeFenced {
		if len(args) < 8 || args[6] != "--" ||
			strings.TrimSpace(args[7]) == "" {
			return guardInvocation{}, errors.New(
				"invalid fenced internal process guard invocation",
			)
		}
		readyFD, err := strconv.Atoi(args[3])
		if err != nil || proofFD != fencedProofFD ||
			readyFD != readyIdentityFD {
			return guardInvocation{}, errors.New(
				"invalid internal process guard readiness descriptor",
			)
		}
		if err := validateDurableIdentifier(
			args[4],
			"durable process execution ID",
		); err != nil {
			return guardInvocation{}, err
		}
		if err := validateDurableIdentifier(
			args[5],
			"durable process receipt ID",
		); err != nil {
			return guardInvocation{}, err
		}
		if args[4] == args[5] {
			return guardInvocation{}, errors.New(
				"durable process execution and receipt IDs must be domain-separated",
			)
		}
		return guardInvocation{
			mode:        mode,
			parentPID:   parentPID,
			proofFD:     proofFD,
			readyFD:     readyFD,
			executionID: args[4],
			receiptID:   args[5],
			command:     args[7],
			args:        append([]string(nil), args[8:]...),
		}, nil
	}
	if mode == guardModeDurable {
		if len(args) < 9 || args[7] != "--" ||
			strings.TrimSpace(args[8]) == "" {
			return guardInvocation{}, errors.New(
				"invalid durable internal process guard invocation",
			)
		}
		readyFD, readyErr := strconv.Atoi(args[3])
		receiptFD, receiptErr := strconv.Atoi(args[4])
		if readyErr != nil || receiptErr != nil ||
			proofFD != fencedProofFD ||
			readyFD != readyIdentityFD ||
			receiptFD != durableReceiptFD {
			return guardInvocation{}, errors.New(
				"invalid durable internal process guard descriptor layout",
			)
		}
		if err := validateDurableIdentifier(
			args[5],
			"durable process execution ID",
		); err != nil {
			return guardInvocation{}, err
		}
		if err := validateDurableIdentifier(
			args[6],
			"durable process receipt ID",
		); err != nil {
			return guardInvocation{}, err
		}
		if args[5] == args[6] {
			return guardInvocation{}, errors.New(
				"durable process execution and receipt IDs must be domain-separated",
			)
		}
		return guardInvocation{
			mode:        mode,
			parentPID:   parentPID,
			proofFD:     proofFD,
			readyFD:     readyFD,
			receiptFD:   receiptFD,
			executionID: args[5],
			receiptID:   args[6],
			command:     args[8],
			args:        append([]string(nil), args[9:]...),
		}, nil
	}
	if mode != guardModeImmediate ||
		proofFD != immediateProofFD ||
		args[3] != "--" || strings.TrimSpace(args[4]) == "" {
		return guardInvocation{}, errors.New(
			"invalid internal process guard invocation",
		)
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
	// NO_NEW_PRIVS is a Linux thread attribute and target Pdeathsig follows
	// the thread that creates the target. Keep the trusted guard lifecycle on
	// one OS thread so the target inherits the sealed privilege boundary and
	// that creating thread cannot disappear while the guard remains alive.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	invocation, err := parseGuardInvocation(args)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 125
	}
	if err := requireLinuxGuardCapability(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 125
	}
	proof := os.NewFile(uintptr(invocation.proofFD), "autogora-teardown-proof")
	if proof == nil {
		_, _ = fmt.Fprintln(os.Stderr, "missing teardown proof descriptor")
		return 125
	}
	if err := setGuardDescriptorCloseOnExec(
		invocation.proofFD,
		"teardown proof",
	); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 125
	}
	defer proof.Close()
	proofInfo, err := proof.Stat()
	if err != nil || proofInfo.Mode()&os.ModeSocket == 0 {
		_, _ = fmt.Fprintln(
			os.Stderr,
			"teardown proof descriptor is not a socket",
		)
		return 125
	}
	parentGone := watchGuardParentLifetime(proof)
	var (
		durableReceipt    *os.File
		durableIdentity   DurableIdentity
		receiptPrivateKey ed25519.PrivateKey
		ready             *os.File
		identityCaptured  bool
	)
	defer func() {
		clear(receiptPrivateKey)
		runtime.KeepAlive(receiptPrivateKey)
	}()
	if invocation.mode == guardModeFenced ||
		invocation.mode == guardModeDurable {
		ready = os.NewFile(
			uintptr(invocation.readyFD),
			"autogora-durable-identity-ready",
		)
		if ready == nil {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"missing durable process identity readiness descriptor",
			)
			return 125
		}
		if err := setGuardDescriptorCloseOnExec(
			invocation.readyFD,
			"identity readiness",
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 125
		}
		if err := setGuardDescriptorCloseOnExec(
			startFenceFD,
			"start fence",
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 125
		}
		defer ready.Close()
		info, statErr := ready.Stat()
		if statErr != nil || info.Mode()&os.ModeSocket == 0 {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"durable process identity readiness descriptor is not a socket",
			)
			return 125
		}
	}
	if invocation.mode == guardModeDurable {
		durableReceipt = os.NewFile(
			uintptr(invocation.receiptFD),
			"autogora-durable-teardown-receipt",
		)
		if durableReceipt == nil {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"missing durable teardown receipt descriptor",
			)
			return 125
		}
		if err := setGuardDescriptorCloseOnExec(
			invocation.receiptFD,
			"durable teardown receipt",
		); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 125
		}
		defer durableReceipt.Close()
	}
	released := invocation.mode == guardModeImmediate
	attest := func(code int) int {
		receiptErr := error(nil)
		if durableReceipt != nil && identityCaptured {
			if err := verifyLinuxGuardNondumpable(); err != nil {
				receiptErr = err
			} else {
				signed, signErr := signDurableTeardownReceipt(
					DurableTeardownReceipt{
						Version:   DurableTeardownReceiptVersion,
						Identity:  durableIdentity,
						Released:  released,
						Quiescent: true,
					},
					receiptPrivateKey,
				)
				if signErr != nil {
					receiptErr = signErr
				} else {
					receiptErr = writeDurableTeardownReceipt(
						durableReceipt,
						signed,
					)
				}
			}
			if receiptErr != nil {
				receiptErr = errors.Join(
					receiptErr,
					invalidateDurableTeardownReceipt(durableReceipt),
				)
				_, _ = fmt.Fprintln(os.Stderr, receiptErr)
			}
		}
		if _, err := proof.Write([]byte{'Q'}); err != nil {
			return 126
		}
		if receiptErr != nil {
			return 126
		}
		return code
	}
	signals, stopSignals := watchLinuxGuardTerminationSignals(
		invocation.parentPID,
	)
	defer stopSignals()
	if os.Getenv(testStopBeforeProofReady) == "1" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	}
	if _, err := proof.Write([]byte{'R'}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "publish process guard readiness:", err)
		return 125
	}
	if durableReceipt != nil {
		if err := unix.Flock(
			int(durableReceipt.Fd()),
			unix.LOCK_EX|unix.LOCK_NB,
		); err != nil {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"durable teardown receipt lock was lost:",
				err,
			)
			return attest(125)
		}
		if err := validateDurableReceiptSink(durableReceipt); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return attest(125)
		}
	}
	if err := establishLinuxGuard(invocation.parentPID); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return attest(125)
	}

	if invocation.mode == guardModeFenced ||
		invocation.mode == guardModeDurable {
		unsealedIdentity, captureErr := captureLinuxDurableIdentity(
			os.Getpid(),
			invocation.executionID,
			invocation.receiptID,
			linuxUnsealedReceiptPublicKey,
		)
		if captureErr != nil {
			_, _ = fmt.Fprintln(os.Stderr, captureErr)
			return attest(125)
		}
		if os.Getenv(testStopBeforeReadyEnvironment) == "1" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		}
		durableIdentity, receiptPrivateKey, err = sealLinuxReadyIdentity(
			ready,
			unsealedIdentity,
		)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return attest(125)
		}
		identityCaptured = true
		ready = nil
		fenceReleased, interrupted := awaitStartFence(signals, parentGone)
		if !fenceReleased {
			if interrupted != nil {
				return attest(128 + int(interrupted.(syscall.Signal)))
			}
			return attest(125)
		}
		released = true
	} else if err := setLinuxGuardNondumpable(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return attest(125)
	}
	select {
	case received := <-signals:
		return attest(128 + int(received.(syscall.Signal)))
	case <-parentGone:
		return attest(128 + int(syscall.SIGTERM))
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
	case <-parentGone:
		received = syscall.SIGTERM
	}
	if received != nil {
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

func setGuardDescriptorCloseOnExec(fd int, name string) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect %s descriptor flags: %w", name, err)
	}
	if _, err := unix.FcntlInt(
		uintptr(fd),
		unix.F_SETFD,
		flags|unix.FD_CLOEXEC,
	); err != nil {
		return fmt.Errorf("seal %s descriptor: %w", name, err)
	}
	sealed, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("verify %s descriptor flags: %w", name, err)
	}
	if sealed&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("%s descriptor lacks close-on-exec", name)
	}
	return nil
}

func sealLinuxReadyIdentity(
	ready *os.File,
	unsealed DurableIdentity,
) (DurableIdentity, ed25519.PrivateKey, error) {
	if ready == nil {
		return DurableIdentity{}, nil, errors.New(
			"durable process identity readiness socket is unavailable",
		)
	}
	encoded, err := canonicalDurableIdentityJSON(unsealed)
	if err != nil {
		return DurableIdentity{}, nil, err
	}
	if err := writeLinuxReadyPacket(ready, encoded); err != nil {
		_ = ready.Close()
		return DurableIdentity{}, nil, err
	}
	var acknowledgement [1]byte
	count, err := ready.Read(acknowledgement[:])
	if err != nil || count != 1 ||
		string(acknowledgement[:]) != linuxReadyParentACK {
		_ = ready.Close()
		return DurableIdentity{}, nil, errors.New(
			"durable process identity readiness claim was not acknowledged",
		)
	}
	if err := setLinuxGuardNondumpable(); err != nil {
		_ = ready.Close()
		return DurableIdentity{}, nil, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		_ = ready.Close()
		return DurableIdentity{}, nil, fmt.Errorf(
			"generate durable teardown receipt signing key: %w",
			err,
		)
	}
	sealed := unsealed
	sealed.ReceiptPublicKey = hex.EncodeToString(publicKey)
	encoded, err = canonicalDurableIdentityJSON(sealed)
	if err != nil {
		clear(privateKey)
		_ = ready.Close()
		return DurableIdentity{}, nil, err
	}
	if err := writeLinuxReadyPacket(ready, encoded); err != nil {
		clear(privateKey)
		_ = ready.Close()
		return DurableIdentity{}, nil, err
	}
	if err := ready.Close(); err != nil {
		clear(privateKey)
		return DurableIdentity{}, nil, fmt.Errorf(
			"close durable process identity readiness handshake: %w",
			err,
		)
	}
	return sealed, privateKey, nil
}

func writeLinuxReadyPacket(ready *os.File, packet []byte) error {
	count, err := ready.Write(packet)
	if err != nil {
		return fmt.Errorf(
			"write durable process identity readiness handshake: %w",
			err,
		)
	}
	if count != len(packet) {
		return fmt.Errorf(
			"write durable process identity readiness handshake: %w",
			io.ErrShortWrite,
		)
	}
	return nil
}

func setLinuxGuardNondumpable() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("disable process guard dumpability: %w", err)
	}
	if err := unix.Prctl(
		unix.PR_SET_NO_NEW_PRIVS,
		1,
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf(
			"disable guarded target privilege escalation: %w",
			err,
		)
	}
	return verifyLinuxGuardNondumpable()
}

func verifyLinuxGuardNondumpable() error {
	dumpable, err := unix.PrctlRetInt(
		unix.PR_GET_DUMPABLE,
		0,
		0,
		0,
		0,
	)
	if err != nil {
		return fmt.Errorf("inspect process guard dumpability: %w", err)
	}
	if dumpable != 0 {
		return errors.New("process guard unexpectedly remains dumpable")
	}
	noNewPrivileges, err := unix.PrctlRetInt(
		unix.PR_GET_NO_NEW_PRIVS,
		0,
		0,
		0,
		0,
	)
	if err != nil {
		return fmt.Errorf(
			"inspect guarded target privilege boundary: %w",
			err,
		)
	}
	if noNewPrivileges != 1 {
		return errors.New(
			"process guard unexpectedly permits new target privileges",
		)
	}
	return nil
}

func establishLinuxGuard(expectedParent int) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable child subreaper: %w", err)
	}
	// SIGCONT is only a wake-up hint. The guard confirms parent death through
	// proof-socket EOF or a changed kernel parent identity, so an early signal
	// cannot make it tear down a live run.
	if err := unix.Prctl(
		unix.PR_SET_PDEATHSIG,
		uintptr(syscall.SIGCONT),
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf("arm process guard parent wake-up: %w", err)
	}
	if os.Getppid() != expectedParent {
		return errors.New("process guard parent exited during startup")
	}
	return nil
}

func watchLinuxGuardTerminationSignals(
	expectedParent int,
) (<-chan os.Signal, func()) {
	termination := make(chan os.Signal, 4)
	parentWake := make(chan os.Signal, 1)
	filtered := make(chan os.Signal, 4)
	done := make(chan struct{})
	signal.Notify(
		termination,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	)
	signal.Notify(parentWake, syscall.SIGCONT)
	go func() {
		var parentLost sync.Once
		reportParentLoss := func() {
			if os.Getppid() == expectedParent {
				return
			}
			parentLost.Do(func() {
				select {
				case filtered <- syscall.SIGTERM:
				case <-done:
				}
			})
		}
		// Close the registration race if the parent changed immediately before
		// signal.Notify installed the SIGCONT wake channel.
		reportParentLoss()
		for {
			select {
			case received := <-termination:
				select {
				case filtered <- received:
				case <-done:
					return
				}
			case <-parentWake:
				reportParentLoss()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(termination)
			signal.Stop(parentWake)
			close(done)
		})
	}
	return filtered, stop
}

func watchGuardParentLifetime(proof *os.File) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		var value [1]byte
		_, _ = proof.Read(value[:])
		close(gone)
	}()
	return gone
}

func awaitStartFence(
	signals <-chan os.Signal,
	parentGone <-chan struct{},
) (bool, os.Signal) {
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
	case <-parentGone:
		_ = fence.Close()
		return false, syscall.SIGTERM
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
			if observedErr != nil ||
				!observed.sameProcessInstance(expected) {
				_ = unix.Close(fd)
				if observedErr != nil && errors.Is(observedErr, os.ErrNotExist) {
					continue
				}
				complete = false
				continue
			}
			if observed.state == 'Z' {
				_ = unix.Close(fd)
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
	state          byte
	parentPID      int
	processGroupID int
	startTime      uint64
}

func (state linuxProcessStat) sameProcessInstance(
	other linuxProcessStat,
) bool {
	return state.parentPID == other.parentPID &&
		state.processGroupID == other.processGroupID &&
		state.startTime == other.startTime
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
	if len(fields[0]) != 1 {
		return linuxProcessStat{}, errors.New("invalid process state")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return linuxProcessStat{}, err
	}
	processGroupID, err := strconv.Atoi(fields[2])
	if err != nil {
		return linuxProcessStat{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return linuxProcessStat{}, err
	}
	return linuxProcessStat{
		state:          fields[0][0],
		parentPID:      parentPID,
		processGroupID: processGroupID,
		startTime:      startTime,
	}, nil
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
