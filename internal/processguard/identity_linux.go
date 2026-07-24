//go:build linux

package processguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxBootIDPath               = "/proc/sys/kernel/random/boot_id"
	privateDurableReceiptMinFD    = 10
	linuxReadyHandshakeLimit      = 3 * time.Second
	linuxReadyParentACK           = "A"
	linuxUnsealedReceiptPublicKey = "0101010101010101010101010101010101010101010101010101010101010101"
)

type linuxPIDNamespace struct {
	device uint64
	inode  uint64
}

type linuxReceiptInode struct {
	device uint64
	inode  uint64
}

var linuxDurableReceiptClaims = struct {
	sync.Mutex
	claimed map[linuxReceiptInode]*linuxReceiptClaimToken
}{
	claimed: make(map[linuxReceiptInode]*linuxReceiptClaimToken),
}

type linuxReceiptClaimToken struct{}

type linuxDurableReceiptLease struct {
	file   *os.File
	inode  linuxReceiptInode
	token  *linuxReceiptClaimToken
	once   sync.Once
	result error
}

func (lease *linuxDurableReceiptLease) close() error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		if lease.file == nil {
			lease.result = errors.New(
				"durable teardown receipt lease has no descriptor",
			)
			return
		}
		unlockErr := unix.Flock(int(lease.file.Fd()), unix.LOCK_UN)
		closeErr := lease.file.Close()
		lease.result = errors.Join(unlockErr, closeErr)
		if unlockErr != nil {
			// Retain the registry token when the kernel lock could not be
			// released. A later claim must fail closed.
			return
		}
		linuxDurableReceiptClaims.Lock()
		if linuxDurableReceiptClaims.claimed[lease.inode] == lease.token {
			delete(linuxDurableReceiptClaims.claimed, lease.inode)
		}
		linuxDurableReceiptClaims.Unlock()
	})
	return lease.result
}

type linuxIdentityHandshake struct {
	connection  *os.File
	executionID string
	receiptID   string
	closeOnce   sync.Once
	closeErr    error
}

func validateDurableReceiptConfigPlatform(file *os.File) error {
	if file == nil {
		return errors.New(
			"durable teardown receipt source descriptor is required",
		)
	}
	if descriptor := file.Fd(); descriptor < 3 {
		return fmt.Errorf(
			"durable teardown receipt source descriptor must be at least 3, got %d",
			descriptor,
		)
	}
	return validateDurableReceiptSink(file)
}

func (handshake *linuxIdentityHandshake) receive(
	ctx context.Context,
	pid int,
) (DurableIdentity, error) {
	if handshake == nil || handshake.connection == nil {
		return DurableIdentity{}, errors.New(
			"durable process identity readiness socket is unavailable",
		)
	}
	connection := handshake.connection
	type result struct {
		identity DurableIdentity
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		defer handshake.close()
		identity, err := handshake.exchange(connection, pid)
		completed <- result{identity: identity, err: err}
	}()
	timer := time.NewTimer(linuxReadyHandshakeLimit)
	defer timer.Stop()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case received := <-completed:
		return received.identity, received.err
	case <-ctx.Done():
		_ = handshake.close()
		return DurableIdentity{}, fmt.Errorf(
			"durable process identity readiness handshake: %w",
			ctx.Err(),
		)
	case <-timer.C:
		_ = handshake.close()
		return DurableIdentity{}, ErrFencedCommandReadinessTimeout
	}
}

func (handshake *linuxIdentityHandshake) exchange(
	connection *os.File,
	pid int,
) (DurableIdentity, error) {
	raw, err := readLinuxReadyPacket(connection)
	if err != nil {
		return DurableIdentity{}, err
	}
	unsealed, err := parseCanonicalDurableIdentity(raw)
	if err != nil {
		return DurableIdentity{}, err
	}
	if unsealed.GuardPID != pid {
		return DurableIdentity{}, errors.New(
			"durable process identity readiness PID does not match the started guard",
		)
	}
	if unsealed.ExecutionID != handshake.executionID ||
		unsealed.ReceiptID != handshake.receiptID ||
		unsealed.ReceiptPublicKey != linuxUnsealedReceiptPublicKey {
		return DurableIdentity{}, errors.New(
			"durable process identity readiness claim does not match the command",
		)
	}
	observation, err := observeLinuxReadyIdentity(unsealed)
	if err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"verify ready durable process identity: %w",
			err,
		)
	}
	if observation != DurableProcessExactLive {
		return DurableIdentity{}, fmt.Errorf(
			"ready durable process identity is %s",
			observation,
		)
	}
	count, err := connection.Write([]byte(linuxReadyParentACK))
	if err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"acknowledge durable process identity readiness claim: %w",
			err,
		)
	}
	if count != len(linuxReadyParentACK) {
		return DurableIdentity{}, fmt.Errorf(
			"acknowledge durable process identity readiness claim: %w",
			io.ErrShortWrite,
		)
	}
	raw, err = readLinuxReadyPacket(connection)
	if err != nil {
		return DurableIdentity{}, err
	}
	sealed, err := parseCanonicalDurableIdentity(raw)
	if err != nil {
		return DurableIdentity{}, err
	}
	expectedUnsealed := sealed
	expectedUnsealed.ReceiptPublicKey = linuxUnsealedReceiptPublicKey
	if expectedUnsealed != unsealed ||
		sealed.ReceiptPublicKey == linuxUnsealedReceiptPublicKey {
		return DurableIdentity{}, errors.New(
			"sealed durable process identity does not match its verified readiness claim",
		)
	}
	return sealed, nil
}

func (handshake *linuxIdentityHandshake) close() error {
	if handshake == nil || handshake.connection == nil {
		return nil
	}
	handshake.closeOnce.Do(func() {
		handshake.closeErr = handshake.connection.Close()
	})
	return handshake.closeErr
}

func newLinuxIdentityHandshake(
	executionID string,
	receiptID string,
) (*linuxIdentityHandshake, *os.File, error) {
	pair, err := unix.Socketpair(
		unix.AF_UNIX,
		unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"create durable process identity readiness socket: %w",
			err,
		)
	}
	parent := os.NewFile(
		uintptr(pair[0]),
		"autogora-durable-identity-parent",
	)
	child := os.NewFile(
		uintptr(pair[1]),
		"autogora-durable-identity-guard",
	)
	if parent == nil || child == nil {
		if parent != nil {
			_ = parent.Close()
		} else {
			_ = unix.Close(pair[0])
		}
		if child != nil {
			_ = child.Close()
		} else {
			_ = unix.Close(pair[1])
		}
		return nil, nil, errors.New(
			"create durable process identity readiness socket handles",
		)
	}
	return &linuxIdentityHandshake{
		connection:  parent,
		executionID: executionID,
		receiptID:   receiptID,
	}, child, nil
}

func readLinuxReadyPacket(connection *os.File) ([]byte, error) {
	buffer := make([]byte, maxDurableReceiptBytes+1)
	count, err := connection.Read(buffer)
	if err != nil {
		return nil, fmt.Errorf(
			"read durable process identity readiness handshake: %w",
			err,
		)
	}
	if count == 0 {
		return nil, errors.New(
			"durable process identity readiness handshake is empty",
		)
	}
	if count > maxDurableReceiptBytes {
		return nil, errors.New(
			"durable process identity readiness handshake is oversized",
		)
	}
	return buffer[:count], nil
}

func captureLinuxDurableIdentity(
	pid int,
	executionID string,
	receiptID string,
	receiptPublicKey string,
) (DurableIdentity, error) {
	if pid <= 0 {
		return DurableIdentity{}, errors.New(
			"capture durable process identity: guard PID must be positive",
		)
	}
	if err := validateDurableIdentifier(
		executionID,
		"durable process execution ID",
	); err != nil {
		return DurableIdentity{}, err
	}
	if err := validateDurableIdentifier(
		receiptID,
		"durable process receipt ID",
	); err != nil {
		return DurableIdentity{}, err
	}
	if executionID == receiptID {
		return DurableIdentity{}, errors.New(
			"durable process execution and receipt IDs must be domain-separated",
		)
	}
	bootID, err := linuxBootID()
	if err != nil {
		return DurableIdentity{}, err
	}
	namespace, err := linuxProcessPIDNamespace(pid)
	if err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"capture durable process PID namespace: %w",
			err,
		)
	}
	state, err := linuxProcessState(pid)
	if err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"capture durable process state: %w",
			err,
		)
	}
	if state.processGroupID != pid {
		return DurableIdentity{}, errors.New(
			"fenced Linux guard does not lead its process group",
		)
	}
	if state.state == 'Z' {
		return DurableIdentity{}, errors.New(
			"fenced Linux guard exited before its identity was captured",
		)
	}
	guard, err := linuxGuardProcess(
		pid,
		executionID,
		receiptID,
	)
	if err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"verify durable process guard command: %w",
			err,
		)
	}
	if !guard {
		return DurableIdentity{}, errors.New(
			"durable process identity does not identify an Autogora guard",
		)
	}
	identity := DurableIdentity{
		Version:            DurableIdentityVersion,
		BootID:             bootID,
		PIDNamespaceDevice: namespace.device,
		PIDNamespaceInode:  namespace.inode,
		GuardPID:           pid,
		StartTimeTicks:     state.startTime,
		ProcessGroupID:     state.processGroupID,
		ExecutionID:        executionID,
		ReceiptID:          receiptID,
		ReceiptPublicKey:   receiptPublicKey,
	}
	if err := identity.Validate(); err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"validate captured durable process identity: %w",
			err,
		)
	}
	return identity, nil
}

// ObserveDurableIdentity compares a persisted identity with the current Linux
// kernel view. Absent and Reused deliberately do not imply quiescence.
func ObserveDurableIdentity(
	identity DurableIdentity,
) (DurableProcessObservation, error) {
	return observeLinuxDurableIdentity(identity, false)
}

// observeLinuxReadyIdentity performs the additional procfs checks while the
// guard is still dumpable. The duplex readiness protocol acknowledges this
// observation before the guard seals itself and publishes the final identity.
func observeLinuxReadyIdentity(
	identity DurableIdentity,
) (DurableProcessObservation, error) {
	return observeLinuxDurableIdentity(identity, true)
}

func observeLinuxDurableIdentity(
	identity DurableIdentity,
	verifyReadyMetadata bool,
) (DurableProcessObservation, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	bootID, err := linuxBootID()
	if err != nil {
		return "", err
	}
	if bootID != identity.BootID {
		return DurableProcessDifferentBoot, nil
	}
	namespace, err := linuxProcessPIDNamespace(os.Getpid())
	if err != nil {
		return "", fmt.Errorf(
			"inspect current PID namespace: %w",
			err,
		)
	}
	if namespace.device != identity.PIDNamespaceDevice ||
		namespace.inode != identity.PIDNamespaceInode {
		return DurableProcessDifferentNamespace, nil
	}

	// pidfd_open binds the observation to the current PID instance while the
	// subsequent /proc reads compare its kernel start token. The descriptor is
	// closed before returning; a future signal operation must repeat this
	// binding and comparison.
	pidfd, err := unix.PidfdOpen(identity.GuardPID, 0)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist) {
		return DurableProcessAbsent, nil
	}
	if err != nil {
		return "", fmt.Errorf("open durable guard pidfd: %w", err)
	}
	defer unix.Close(pidfd)

	state, err := linuxProcessState(identity.GuardPID)
	if errors.Is(err, os.ErrNotExist) {
		return DurableProcessAbsent, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect durable guard process state: %w", err)
	}
	if state.startTime != identity.StartTimeTicks ||
		state.processGroupID != identity.ProcessGroupID ||
		state.state == 'Z' {
		return DurableProcessReused, nil
	}
	if verifyReadyMetadata {
		processNamespace, err := linuxProcessPIDNamespace(identity.GuardPID)
		if errors.Is(err, os.ErrNotExist) {
			return DurableProcessAbsent, nil
		}
		if err != nil {
			return "", fmt.Errorf(
				"inspect durable guard PID namespace: %w",
				err,
			)
		}
		if processNamespace.device != identity.PIDNamespaceDevice ||
			processNamespace.inode != identity.PIDNamespaceInode {
			return DurableProcessReused, nil
		}
		guard, err := linuxGuardProcess(
			identity.GuardPID,
			identity.ExecutionID,
			identity.ReceiptID,
		)
		if errors.Is(err, os.ErrNotExist) {
			return DurableProcessAbsent, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect durable guard command: %w", err)
		}
		if !guard {
			return DurableProcessReused, nil
		}
	}
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); errors.Is(
		err,
		unix.ESRCH,
	) {
		return DurableProcessAbsent, nil
	} else if err != nil {
		return "", fmt.Errorf(
			"verify durable guard pidfd liveness: %w",
			err,
		)
	}
	return DurableProcessExactLive, nil
}

func linuxBootID() (string, error) {
	raw, err := os.ReadFile(linuxBootIDPath)
	if err != nil {
		return "", fmt.Errorf("read Linux boot ID: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	if !validCanonicalBootID(value) {
		return "", errors.New("Linux boot ID is not canonical")
	}
	return value, nil
}

func linuxProcessPIDNamespace(pid int) (linuxPIDNamespace, error) {
	info, err := os.Stat(filepath.Join(
		"/proc",
		strconv.Itoa(pid),
		"ns",
		"pid",
	))
	if err != nil {
		return linuxPIDNamespace{}, err
	}
	state, ok := info.Sys().(*syscall.Stat_t)
	if !ok || state.Dev == 0 || state.Ino == 0 {
		return linuxPIDNamespace{}, errors.New(
			"PID namespace lacks a stable device or inode",
		)
	}
	return linuxPIDNamespace{
		device: uint64(state.Dev),
		inode:  state.Ino,
	}, nil
}

func linuxGuardProcess(
	pid int,
	executionID string,
	receiptID string,
) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(
		"/proc",
		strconv.Itoa(pid),
		"cmdline",
	))
	if err != nil {
		return false, err
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) < 3 || fields[1] != guardMarker {
		return false, nil
	}
	switch fields[2] {
	case guardModeFenced:
		return len(fields) > 7 &&
			fields[6] == executionID &&
			fields[7] == receiptID, nil
	case guardModeDurable:
		return len(fields) > 8 &&
			fields[7] == executionID &&
			fields[8] == receiptID, nil
	default:
		return false, nil
	}
}

func acquireLinuxDurableReceipt(
	config DurableReceiptConfig,
) (*linuxDurableReceiptLease, error) {
	if err := config.consume(); err != nil {
		return nil, err
	}
	if config.File == nil {
		return nil, errors.New("durable teardown receipt file is required")
	}
	info, err := config.File.Stat()
	if err != nil {
		return nil, fmt.Errorf(
			"inspect durable teardown receipt inode: %w",
			err,
		)
	}
	state, ok := info.Sys().(*syscall.Stat_t)
	if !ok || state.Ino == 0 {
		return nil, errors.New(
			"durable teardown receipt lacks a stable device and inode",
		)
	}
	key := linuxReceiptInode{
		device: uint64(state.Dev),
		inode:  state.Ino,
	}
	token := &linuxReceiptClaimToken{}
	linuxDurableReceiptClaims.Lock()
	if _, exists := linuxDurableReceiptClaims.claimed[key]; exists {
		linuxDurableReceiptClaims.Unlock()
		return nil, ErrDurableReceiptAlreadyClaimed
	}
	linuxDurableReceiptClaims.claimed[key] = token
	linuxDurableReceiptClaims.Unlock()

	var private *os.File
	releaseFailedSetup := func(setupErr error, locked bool) (
		*linuxDurableReceiptLease,
		error,
	) {
		var unlockErr error
		if locked && private != nil {
			unlockErr = unix.Flock(
				int(private.Fd()),
				unix.LOCK_UN,
			)
		}
		var closeErr error
		if private != nil {
			closeErr = private.Close()
			private = nil
		}
		if !locked || unlockErr == nil {
			linuxDurableReceiptClaims.Lock()
			if linuxDurableReceiptClaims.claimed[key] == token {
				delete(linuxDurableReceiptClaims.claimed, key)
			}
			linuxDurableReceiptClaims.Unlock()
		}
		return nil, errors.Join(setupErr, unlockErr, closeErr)
	}

	if err := validateDurableReceiptConfigPlatform(config.File); err != nil {
		return releaseFailedSetup(err, false)
	}
	private, err = openIndependentDurableReceiptFile(config.File, key)
	if err != nil {
		return releaseFailedSetup(err, false)
	}
	if err := unix.Flock(
		int(private.Fd()),
		unix.LOCK_EX|unix.LOCK_NB,
	); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return releaseFailedSetup(fmt.Errorf(
				"%w: receipt inode is locked by another process",
				ErrDurableReceiptAlreadyClaimed,
			), false)
		}
		return releaseFailedSetup(fmt.Errorf(
			"lock durable teardown receipt inode: %w",
			err,
		), false)
	}
	// Close the validate-to-lock race. Advisory locking cannot stop a writer
	// that deliberately ignores the lock, so the guard validates once more
	// before publishing readiness.
	if err := validateDurableReceiptSink(private); err != nil {
		return releaseFailedSetup(err, true)
	}
	lockedInfo, err := private.Stat()
	if err != nil {
		return releaseFailedSetup(fmt.Errorf(
			"reinspect locked durable teardown receipt inode: %w",
			err,
		), true)
	}
	lockedState, ok := lockedInfo.Sys().(*syscall.Stat_t)
	if !ok ||
		uint64(lockedState.Dev) != key.device ||
		lockedState.Ino != key.inode {
		return releaseFailedSetup(errors.New(
			"durable teardown receipt inode changed while it was claimed",
		), true)
	}
	// A caller-owned source descriptor is a separate open file description.
	// Re-check its identity if it remains open, but never acquire or release
	// the library lease through it.
	sourceInfo, err := config.File.Stat()
	if err != nil {
		return releaseFailedSetup(fmt.Errorf(
			"reinspect durable teardown receipt source inode: %w",
			err,
		), true)
	}
	if !os.SameFile(info, sourceInfo) {
		return releaseFailedSetup(errors.New(
			"durable teardown receipt source identity changed while it was claimed",
		), true)
	}
	return &linuxDurableReceiptLease{
		file:  private,
		inode: key,
		token: token,
	}, nil
}

func openIndependentDurableReceiptFile(
	source *os.File,
	expected linuxReceiptInode,
) (*os.File, error) {
	if err := validateDurableReceiptConfigPlatform(source); err != nil {
		return nil, err
	}
	reopenedFD, err := unix.Open(
		filepath.Join("/proc/self/fd", strconv.FormatUint(
			uint64(source.Fd()),
			10,
		)),
		unix.O_RDWR|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"open independent durable teardown receipt descriptor: %w",
			err,
		)
	}
	descriptor, err := unix.FcntlInt(
		uintptr(reopenedFD),
		unix.F_DUPFD_CLOEXEC,
		privateDurableReceiptMinFD,
	)
	closeReopenedErr := unix.Close(reopenedFD)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf(
				"normalize independent durable teardown receipt descriptor: %w",
				err,
			),
			closeReopenedErr,
		)
	}
	if closeReopenedErr != nil {
		_ = unix.Close(descriptor)
		return nil, fmt.Errorf(
			"close temporary durable teardown receipt descriptor: %w",
			closeReopenedErr,
		)
	}
	private := os.NewFile(
		uintptr(descriptor),
		"autogora-durable-teardown-receipt",
	)
	if private == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New(
			"create durable teardown receipt file handle",
		)
	}
	if private.Fd() < privateDurableReceiptMinFD {
		_ = private.Close()
		return nil, errors.New(
			"private durable teardown receipt descriptor is below its reserved range",
		)
	}
	if err := validateDurableReceiptSink(private); err != nil {
		_ = private.Close()
		return nil, err
	}
	info, err := private.Stat()
	if err != nil {
		_ = private.Close()
		return nil, fmt.Errorf(
			"inspect independent durable teardown receipt inode: %w",
			err,
		)
	}
	state, ok := info.Sys().(*syscall.Stat_t)
	if !ok ||
		uint64(state.Dev) != expected.device ||
		state.Ino != expected.inode {
		_ = private.Close()
		return nil, errors.New(
			"independent durable teardown receipt inode does not match its source",
		)
	}
	return private, nil
}

func validateDurableReceiptSink(file *os.File) error {
	if file == nil {
		return ErrDurableTeardownReceiptUnavailable
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect durable teardown receipt sink: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("durable teardown receipt sink is not regular")
	}
	if info.Size() != 0 {
		return errors.New("durable teardown receipt sink is not empty")
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		return errors.New(
			"durable teardown receipt sink permissions must be 0600",
		)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New(
			"durable teardown receipt sink uses special permission bits",
		)
	}
	state, ok := info.Sys().(*syscall.Stat_t)
	if !ok || state.Nlink == 0 {
		return errors.New(
			"durable teardown receipt sink is unlinked or lacks stable metadata",
		)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf(
			"inspect durable teardown receipt sink descriptor: %w",
			err,
		)
	}
	if flags&unix.O_ACCMODE == unix.O_RDONLY {
		return errors.New("durable teardown receipt sink is not writable")
	}
	if flags&unix.O_APPEND != 0 {
		return errors.New(
			"durable teardown receipt sink cannot use append mode",
		)
	}
	descriptorFlags, err := unix.FcntlInt(
		file.Fd(),
		unix.F_GETFD,
		0,
	)
	if err != nil {
		return fmt.Errorf(
			"inspect durable teardown receipt descriptor flags: %w",
			err,
		)
	}
	if descriptorFlags&unix.FD_CLOEXEC == 0 {
		return errors.New(
			"durable teardown receipt descriptor must use close-on-exec",
		)
	}
	return nil
}

func writeDurableTeardownReceipt(
	file *os.File,
	receipt DurableTeardownReceipt,
) error {
	if file == nil {
		return ErrDurableTeardownReceiptUnavailable
	}
	encoded, err := receipt.CanonicalJSON()
	if err != nil {
		return err
	}
	written := 0
	for written < len(encoded) {
		count, writeErr := file.WriteAt(
			encoded[written:],
			int64(written),
		)
		written += count
		if writeErr != nil {
			return fmt.Errorf("write durable teardown receipt: %w", writeErr)
		}
		if count == 0 {
			return fmt.Errorf(
				"write durable teardown receipt: %w",
				io.ErrShortWrite,
			)
		}
	}
	if err := file.Truncate(int64(len(encoded))); err != nil {
		return fmt.Errorf("truncate durable teardown receipt: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync durable teardown receipt: %w", err)
	}
	return nil
}

func invalidateDurableTeardownReceipt(file *os.File) error {
	if file == nil {
		return ErrDurableTeardownReceiptUnavailable
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("invalidate durable teardown receipt: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync invalid durable teardown receipt: %w", err)
	}
	return nil
}
