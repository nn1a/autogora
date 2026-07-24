//go:build linux

package processguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	durableFDHelperEnvironment = "AUTOGORA_TEST_DURABLE_FD_HELPER"
	durableFDReceiptPath       = "AUTOGORA_TEST_DURABLE_FD_RECEIPT_PATH"
	durableClaimHelper         = "AUTOGORA_TEST_DURABLE_CLAIM_HELPER"
	parentSocketHelper         = "AUTOGORA_TEST_PARENT_SOCKET_HELPER"
	parentSocketPID            = "AUTOGORA_TEST_PARENT_SOCKET_PID"
	parentSocketFD             = "AUTOGORA_TEST_PARENT_SOCKET_FD"
	durableParentDeathHelper   = "AUTOGORA_TEST_DURABLE_PARENT_DEATH_HELPER"
	durableStoppedParentHelper = "AUTOGORA_TEST_DURABLE_STOPPED_PARENT_HELPER"
	durableIdentityPath        = "AUTOGORA_TEST_DURABLE_IDENTITY_PATH"
	durableProofHolderPIDPath  = "AUTOGORA_TEST_DURABLE_PROOF_HOLDER_PID_PATH"
	durableStoppedMarkerPath   = "AUTOGORA_TEST_DURABLE_STOPPED_MARKER_PATH"
)

func openDurableReceipt(t *testing.T) (*os.File, string, DurableReceiptConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "teardown-receipt.json")
	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewDurableReceiptConfig(file)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	return file, path, config
}

func readDurableReceipt(
	t *testing.T,
	path string,
) DurableTeardownReceipt {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ParseDurableTeardownReceipt(raw)
	if err != nil {
		t.Fatalf("parse durable receipt %q: %v", raw, err)
	}
	return receipt
}

func fencedPID(t *testing.T, command *FencedCommand) int {
	t.Helper()
	pid, err := command.PID()
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestLinuxFencedStartReturnsReadyExactIdentityBeforeRelease(
	t *testing.T,
) {
	marker := filepath.Join(t.TempDir(), "target-started")
	command, err := NewFencedCommandContext(
		context.Background(),
		5*time.Second,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if _, err := command.Identity(); !errors.Is(
		err,
		ErrFencedCommandNotStarted,
	) {
		t.Fatalf("identity before Start error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.GuardPID != fencedPID(t, command) {
		t.Fatalf(
			"identity guard PID = %d, process PID = %d",
			identity.GuardPID,
			fencedPID(t, command),
		)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	observation, err := ObserveDurableIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if observation != DurableProcessExactLive {
		t.Fatalf("live identity observation = %s", observation)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed the start fence before Release: %v", err)
	}
	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("aborted guard unexpectedly succeeded")
	}
}

func TestLinuxFencedReadinessStopIsBoundedAndNeverStartsTarget(
	t *testing.T,
) {
	t.Setenv(testStopBeforeReadyEnvironment, "1")
	marker := filepath.Join(t.TempDir(), "target-started")
	command, err := NewFencedCommandContext(
		context.Background(),
		0,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()

	started := time.Now()
	err = command.Start()
	elapsed := time.Since(started)
	if !errors.Is(err, ErrFencedCommandReadinessTimeout) {
		t.Fatalf("stopped readiness error = %v", err)
	}
	if elapsed > linuxReadyHandshakeLimit+teardownConfirmationLimit {
		t.Fatalf("stopped readiness took %s", elapsed)
	}
	if command.ProcessState() == nil {
		t.Fatal("stopped readiness guard was not reaped")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target crossed a failed readiness fence: %v", statErr)
	}
}

func TestLinuxFencedProofReadinessStopIsRecoveredWithoutQuarantine(
	t *testing.T,
) {
	t.Setenv(testStopBeforeProofReady, "1")
	marker := filepath.Join(t.TempDir(), "target-started")
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		0,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()

	started := time.Now()
	err = command.Start()
	if elapsed := time.Since(started); elapsed >
		linuxReadyHandshakeLimit+teardownConfirmationLimit {
		t.Fatalf("proof readiness recovery took %s", elapsed)
	}
	if err == nil {
		t.Fatal("stopped proof readiness unexpectedly succeeded")
	}
	if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("stopped proof readiness lost teardown proof: %v", err)
	}
	if command.ProcessState() == nil {
		t.Fatal("stopped proof readiness guard was not reaped")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target crossed a failed proof readiness fence: %v", statErr)
	}
	if reported {
		t.Fatal("unreleased proof readiness failure caused quarantine")
	}
}

func TestLinuxFencedCloseCancelsStoppedReadinessPromptly(t *testing.T) {
	t.Setenv(testStopBeforeReadyEnvironment, "1")
	marker := filepath.Join(t.TempDir(), "target-started")
	reported := make(chan error, 1)
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(err error) { reported <- err },
	)
	command, err := NewFencedCommandContext(
		ctx,
		0,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	startResult := make(chan error, 1)
	go func() {
		startResult <- command.Start()
	}()
	deadline := time.Now().Add(fencedTestWaitLimit)
	for {
		command.mu.Lock()
		starting := command.state == fencedCommandStarting
		command.mu.Unlock()
		if starting {
			break
		}
		if time.Now().After(deadline) {
			command.Close()
			t.Fatal("fenced command did not enter readiness")
		}
		time.Sleep(time.Millisecond)
	}

	started := time.Now()
	command.Close()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Close took %s during stopped readiness", elapsed)
	}
	if err := <-startResult; !errors.Is(err, ErrFencedCommandClosed) {
		t.Fatalf("concurrent Start error = %v", err)
	} else if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("concurrent Close lost an unreleased guard proof: %v", err)
	}
	if command.ProcessState() == nil {
		t.Fatal("concurrently closed readiness guard was not reaped")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target crossed a concurrently closed readiness fence: %v", statErr)
	}
	select {
	case err := <-reported:
		t.Fatalf("unreleased concurrent Close reported teardown failure: %v", err)
	default:
	}
}

func TestLinuxFencedContextCancellationDuringStoppedReadinessIsConfirmed(
	t *testing.T,
) {
	t.Setenv(testStopBeforeReadyEnvironment, "1")
	marker := filepath.Join(t.TempDir(), "target-started")
	reported := make(chan error, 1)
	parent, cancel := context.WithCancel(context.Background())
	ctx := WithTeardownFailureReporter(
		parent,
		func(err error) { reported <- err },
	)
	command, err := NewFencedCommandContext(
		ctx,
		0,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	startResult := make(chan error, 1)
	go func() {
		startResult <- command.Start()
	}()
	deadline := time.Now().Add(fencedTestWaitLimit)
	for {
		command.mu.Lock()
		starting := command.state == fencedCommandStarting
		command.mu.Unlock()
		if starting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fenced command did not enter stopped readiness")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-startResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled readiness error = %v", err)
		}
		if errors.Is(err, ErrTeardownUnconfirmed) {
			t.Fatalf("canceled readiness lost teardown proof: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled readiness remained blocked")
	}
	if command.ProcessState() == nil {
		t.Fatal("canceled readiness guard was not reaped")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed a canceled readiness fence: %v", err)
	}
	select {
	case err := <-reported:
		t.Fatalf("canceled unreleased guard reported teardown failure: %v", err)
	default:
	}
}

func TestLinuxFencedAbortCancelsStoppedReadinessWithoutReporter(
	t *testing.T,
) {
	t.Setenv(testStopBeforeReadyEnvironment, "1")
	marker := filepath.Join(t.TempDir(), "target-started")
	reported := make(chan error, 1)
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(err error) { reported <- err },
	)
	command, err := NewFencedCommandContext(
		ctx,
		0,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	startResult := make(chan error, 1)
	go func() {
		startResult <- command.Start()
	}()
	deadline := time.Now().Add(fencedTestWaitLimit)
	for {
		command.mu.Lock()
		starting := command.state == fencedCommandStarting
		command.mu.Unlock()
		if starting {
			break
		}
		if time.Now().After(deadline) {
			command.Close()
			t.Fatal("fenced command did not enter readiness")
		}
		time.Sleep(time.Millisecond)
	}

	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	if err := <-startResult; !errors.Is(
		err,
		ErrFencedCommandStartAborted,
	) {
		t.Fatalf("concurrent Start error = %v", err)
	} else if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("concurrent Abort lost an unreleased guard proof: %v", err)
	}
	if command.ProcessState() == nil {
		t.Fatal("concurrently aborted readiness guard was not reaped")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target crossed a concurrently aborted readiness fence: %v", statErr)
	}
	select {
	case err := <-reported:
		t.Fatalf("unreleased concurrent Abort reported teardown failure: %v", err)
	default:
	}
}

func TestLinuxDurableReceiptRecordsUnreleasedQuiescence(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.ExecutionID != config.ExecutionID ||
		identity.ReceiptID != config.ReceiptID {
		t.Fatalf("identity IDs do not match durable receipt config: %#v", identity)
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("receipt was written before terminal quiescence: %v, %#v", err, info)
	}
	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("aborted guard unexpectedly succeeded")
	}
	receipt := readDurableReceipt(t, path)
	if receipt.Identity != identity ||
		receipt.Released ||
		!receipt.Quiescent {
		t.Fatalf("unreleased durable receipt = %#v", receipt)
	}
}

func TestLinuxDurableReceiptRecordsReleasedQuiescence(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	receipt := readDurableReceipt(t, path)
	if receipt.Identity != identity ||
		!receipt.Released ||
		!receipt.Quiescent {
		t.Fatalf("released durable receipt = %#v", receipt)
	}
}

func TestLinuxReleaseCommitDoesNotForgeDurableReceipt(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		time.Minute,
		config,
		"/bin/sleep",
		"30",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := syscall.Kill(
		fencedPID(t, command),
		syscall.SIGKILL,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("SIGKILL wait error = %v, want ErrTeardownUnconfirmed", err)
	}
	if raw, err := os.ReadFile(path); err != nil || len(raw) != 0 {
		t.Fatalf("release commit forged a durable receipt: %q, %v", raw, err)
	}
}

func TestLinuxDurableReceiptIsNotInheritedByTarget(t *testing.T) {
	if os.Getenv(durableFDHelperEnvironment) == "1" {
		noNewPrivileges, err := unix.PrctlRetInt(
			unix.PR_GET_NO_NEW_PRIVS,
			0,
			0,
			0,
			0,
		)
		if err != nil || noNewPrivileges != 1 {
			t.Fatalf(
				"target no-new-privileges = %d, %v",
				noNewPrivileges,
				err,
			)
		}
		for _, descriptor := range []int{fencedProofFD, durableReceiptFD} {
			path := filepath.Join(
				"/proc",
				fmt.Sprint(os.Getppid()),
				"fd",
				fmt.Sprint(descriptor),
			)
			leaked, openErr := os.Open(path)
			if openErr == nil {
				_ = leaked.Close()
				t.Fatalf("target opened nondumpable guard descriptor %d", descriptor)
			}
			if !os.IsPermission(openErr) {
				t.Fatalf(
					"open nondumpable guard descriptor %d: %v",
					descriptor,
					openErr,
				)
			}
		}
		receiptInfo, err := os.Stat(os.Getenv(durableFDReceiptPath))
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			info, statErr := os.Stat(filepath.Join(
				"/proc/self/fd",
				entry.Name(),
			))
			if statErr == nil && os.SameFile(receiptInfo, info) {
				t.Fatalf(
					"target inherited durable receipt as fd %s",
					entry.Name(),
				)
			}
		}
		return
	}

	file, path, config := openDurableReceipt(t)
	defer file.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(durableFDHelperEnvironment, "1")
	t.Setenv(durableFDReceiptPath, path)
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		executable,
		"-test.run=^TestLinuxDurableReceiptIsNotInheritedByTarget$",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	receipt := readDurableReceipt(t, path)
	if !receipt.Released || !receipt.Quiescent {
		t.Fatalf("target FD test receipt = %#v", receipt)
	}
}

func TestLinuxGuardSocketsCannotBeReopenedThroughParentProc(t *testing.T) {
	if os.Getenv(parentSocketHelper) == "1" {
		pid, pidErr := strconv.Atoi(os.Getenv(parentSocketPID))
		fd, fdErr := strconv.Atoi(os.Getenv(parentSocketFD))
		if pidErr != nil || fdErr != nil || pid <= 0 || fd < 3 {
			t.Fatalf("invalid parent socket helper values: pid=%d fd=%d", pid, fd)
		}
		path := filepath.Join(
			"/proc",
			strconv.Itoa(pid),
			"fd",
			strconv.Itoa(fd),
		)
		reopened, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			_ = reopened.Close()
			t.Fatalf("reopened parent guard socket %s", path)
		}
		return
	}

	command, err := NewFencedCommandContext(
		context.Background(),
		5*time.Second,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	proof, ok := command.proof.(*linuxTeardownProof)
	if !ok {
		t.Fatalf("teardown proof = %T", command.proof)
	}
	releaseFD := int(command.writer.Fd())
	proofFD := int(proof.reader.Fd())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range []int{releaseFD, proofFD} {
		helper := exec.Command(
			executable,
			"-test.run=^TestLinuxGuardSocketsCannotBeReopenedThroughParentProc$",
		)
		helper.Env = append(
			os.Environ(),
			parentSocketHelper+"=1",
			parentSocketPID+"="+strconv.Itoa(os.Getpid()),
			parentSocketFD+"="+strconv.Itoa(fd),
		)
		if output, err := helper.CombinedOutput(); err != nil {
			t.Fatalf("parent socket helper for fd %d: %v: %s", fd, err, output)
		}
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDurableReceiptRejectsTargetForgeryAndOverwritesIt(
	t *testing.T,
) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	marker := filepath.Join(t.TempDir(), "forgery-written")
	continueMarker := filepath.Join(t.TempDir(), "continue")
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/sh",
		"-c",
		`printf '{"forged":true}' >"$1"; printf ready >"$2"; while [ ! -e "$3" ]; do sleep 0.01; done`,
		"autogora-forgery-test",
		path,
		marker,
		continueMarker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	defer os.WriteFile(continueMarker, []byte("continue"), 0o600)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	requireFileEventually(t, marker)
	forged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDurableTeardownReceipt(forged); err == nil {
		t.Fatalf("target-forged receipt was accepted: %s", forged)
	}
	if err := os.WriteFile(
		continueMarker,
		[]byte("continue"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	final := readDurableReceipt(t, path)
	if final.Identity != identity || !final.Released || !final.Quiescent {
		t.Fatalf("final signed receipt = %#v", final)
	}
}

func TestLinuxDurableReceiptSurvivesParentDeath(t *testing.T) {
	if os.Getenv(durableParentDeathHelper) == "1" {
		path := os.Getenv(durableFDReceiptPath)
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		config, err := NewDurableReceiptConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		command, err := NewFencedCommandContextWithDurableReceipt(
			context.Background(),
			time.Minute,
			config,
			"/bin/true",
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		identity, err := command.Identity()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := canonicalDurableIdentityJSON(identity)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			os.Getenv(durableIdentityPath),
			encoded,
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}

	directory := t.TempDir()
	receiptPath := filepath.Join(directory, "receipt.json")
	receipt, err := os.OpenFile(
		receiptPath,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Close(); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(directory, "identity.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(
		executable,
		"-test.run=^TestLinuxDurableReceiptSurvivesParentDeath$",
	)
	helper.Env = append(
		os.Environ(),
		durableParentDeathHelper+"=1",
		durableFDReceiptPath+"="+receiptPath,
		durableIdentityPath+"="+identityPath,
	)
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("durable parent-death helper: %v: %s", err, output)
	}
	identityRaw, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseCanonicalDurableIdentity(identityRaw)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(fencedTestWaitLimit)
	for {
		raw, readErr := os.ReadFile(receiptPath)
		if readErr == nil && len(raw) != 0 {
			final, parseErr := ParseDurableTeardownReceiptForIdentity(
				raw,
				identity,
			)
			if parseErr != nil {
				t.Fatalf("parent-death receipt %q: %v", raw, parseErr)
			}
			if final.Released || !final.Quiescent {
				t.Fatalf("parent-death receipt = %#v", final)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"parent-death receipt was not written: read=%v",
				readErr,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinuxStoppedGuardWakesAndAttestsAfterParentDeath(t *testing.T) {
	if os.Getenv(durableStoppedParentHelper) == "1" {
		path := os.Getenv(durableFDReceiptPath)
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		config, err := NewDurableReceiptConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		command, err := NewFencedCommandContextWithDurableReceipt(
			context.Background(),
			time.Minute,
			config,
			"/bin/sh",
			"-c",
			`kill -STOP "$PPID"; printf stopped >"$1"; sleep 30`,
			"autogora-target",
			os.Getenv(durableStoppedMarkerPath),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		identity, err := command.Identity()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := canonicalDurableIdentityJSON(identity)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			os.Getenv(durableIdentityPath),
			encoded,
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		proof, ok := command.proof.(*linuxTeardownProof)
		if !ok {
			t.Fatalf("unexpected teardown proof type %T", command.proof)
		}
		duplicate, err := unix.Dup(int(proof.reader.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		unix.CloseOnExec(duplicate)
		proofHolderFile := os.NewFile(
			uintptr(duplicate),
			"autogora-test-proof-holder",
		)
		if proofHolderFile == nil {
			_ = unix.Close(duplicate)
			t.Fatal("create proof holder file")
		}
		proofHolder := exec.Command("/bin/sleep", "30")
		proofHolder.ExtraFiles = []*os.File{proofHolderFile}
		if err := proofHolder.Start(); err != nil {
			_ = proofHolderFile.Close()
			t.Fatal(err)
		}
		if err := proofHolderFile.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			os.Getenv(durableProofHolderPIDPath),
			[]byte(strconv.Itoa(proofHolder.Process.Pid)),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if released, err := command.Release(); err != nil || !released {
			t.Fatalf("release = %t, %v", released, err)
		}
		requireFileEventually(t, os.Getenv(durableStoppedMarkerPath))
		select {}
	}

	directory := t.TempDir()
	receiptPath := filepath.Join(directory, "receipt.json")
	receipt, err := os.OpenFile(
		receiptPath,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Close(); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(directory, "identity.json")
	proofHolderPIDPath := filepath.Join(directory, "proof-holder.pid")
	stoppedPath := filepath.Join(directory, "guard-stopped")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(
		executable,
		"-test.run=^TestLinuxStoppedGuardWakesAndAttestsAfterParentDeath$",
	)
	helper.Env = append(
		os.Environ(),
		durableStoppedParentHelper+"=1",
		durableFDReceiptPath+"="+receiptPath,
		durableIdentityPath+"="+identityPath,
		durableProofHolderPIDPath+"="+proofHolderPIDPath,
		durableStoppedMarkerPath+"="+stoppedPath,
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if helper.Process != nil {
			_ = helper.Process.Kill()
		}
	}()
	requireFileEventually(t, identityPath)
	requireFileEventually(t, proofHolderPIDPath)
	requireFileEventually(t, stoppedPath)
	rawProofHolderPID, err := os.ReadFile(proofHolderPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	proofHolderPID, err := strconv.Atoi(
		strings.TrimSpace(string(rawProofHolderPID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(proofHolderPID, syscall.SIGKILL)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("killed stopped-guard parent unexpectedly succeeded")
	}

	rawIdentity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseCanonicalDurableIdentity(rawIdentity)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(fencedTestWaitLimit)
	for {
		raw, readErr := os.ReadFile(receiptPath)
		if readErr == nil && len(raw) != 0 {
			final, parseErr := ParseDurableTeardownReceiptForIdentity(
				raw,
				identity,
			)
			if parseErr != nil {
				t.Fatalf("stopped parent-death receipt %q: %v", raw, parseErr)
			}
			if !final.Released || !final.Quiescent {
				t.Fatalf("stopped parent-death receipt = %#v", final)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"stopped parent-death receipt was not written: read=%v",
				readErr,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinuxDurableCommandRejectsGuardConfigurationMutation(
	t *testing.T,
) {
	tests := map[string]func(*exec.Cmd){
		"descriptor layout": func(command *exec.Cmd) {
			command.ExtraFiles = command.ExtraFiles[:len(command.ExtraFiles)-1]
		},
		"process attributes": func(command *exec.Cmd) {
			command.SysProcAttr.Setpgid = false
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			file, path, config := openDurableReceipt(t)
			defer file.Close()
			command, err := NewFencedCommandContextWithDurableReceipt(
				context.Background(),
				5*time.Second,
				config,
				"/bin/true",
			)
			if err != nil {
				t.Fatal(err)
			}
			defer command.Close()
			mutate(command.command)
			if err := command.Start(); !errors.Is(
				err,
				ErrFencedCommandConfigurationChanged,
			) {
				t.Fatalf("mutated durable Start error = %v", err)
			}
			if _, pidErr := command.PID(); pidErr == nil {
				t.Fatal("mutated durable command spawned a guard")
			}
			if raw, err := os.ReadFile(path); err != nil || len(raw) != 0 {
				t.Fatalf("mutated command wrote receipt: %q, %v", raw, err)
			}
		})
	}
}

func TestLinuxDurableReceiptRequiresWritableEmptyRegularFile(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *os.File{
		"directory": func(t *testing.T) *os.File {
			file, err := os.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return file
		},
		"nonempty": func(t *testing.T) *os.File {
			path := filepath.Join(t.TempDir(), "receipt")
			if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			return file
		},
		"readonly": func(t *testing.T) *os.File {
			path := filepath.Join(t.TempDir(), "receipt")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			return file
		},
		"append": func(t *testing.T) *os.File {
			path := filepath.Join(t.TempDir(), "receipt")
			file, err := os.OpenFile(
				path,
				os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND,
				0o600,
			)
			if err != nil {
				t.Fatal(err)
			}
			return file
		},
	} {
		t.Run(name, func(t *testing.T) {
			file := open(t)
			defer file.Close()
			config, err := NewDurableReceiptConfig(file)
			if err == nil {
				var command *FencedCommand
				command, err = NewFencedCommandContextWithDurableReceipt(
					context.Background(),
					5*time.Second,
					config,
					"/bin/true",
				)
				if command != nil {
					command.Close()
				}
			}
			if err == nil {
				t.Fatal("invalid durable receipt file was accepted")
			}
		})
	}
}

func TestLinuxDurableReceiptRequiresSealedPrivateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt")
	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	executionID, err := NewDurableIdentifier()
	if err != nil {
		t.Fatal(err)
	}
	receiptID, err := NewDurableIdentifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		DurableReceiptConfig{
			File:        file,
			ExecutionID: executionID,
			ReceiptID:   receiptID,
		},
		"/bin/true",
	); err == nil {
		t.Fatal("unsealed durable receipt config literal was accepted")
	}
}

func TestLinuxDurableReceiptRequiresPrivatePermissions(t *testing.T) {
	for _, permissions := range []os.FileMode{0o640, 0o604, 0o700} {
		t.Run(fmt.Sprintf("%04o", permissions), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt")
			file, err := os.OpenFile(
				path,
				os.O_CREATE|os.O_EXCL|os.O_RDWR,
				0o600,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := file.Chmod(permissions); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDurableReceiptConfig(file); err == nil {
				t.Fatalf("permissions %04o were accepted", permissions)
			}
		})
	}
}

func TestLinuxDurableSIGKILLLeavesNoReceipt(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		time.Minute,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(
		fencedPID(t, command),
		syscall.SIGKILL,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("SIGKILL wait error = %v, want ErrTeardownUnconfirmed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("SIGKILL guard wrote a receipt: %q", raw)
	}
}

func TestLinuxDurableReceiptIdentityCannotBeSubstituted(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	receipt := readDurableReceipt(t, path)
	receipt.Identity.ReceiptID = receipt.Identity.ExecutionID
	if _, err := receipt.CanonicalJSON(); err == nil {
		t.Fatal("substituted receipt identity was accepted")
	}
	if receipt.Identity == identity {
		t.Fatal("test did not substitute the durable identity")
	}
}

func TestLinuxFencedCommandCopiesSafeConfigurationAtStart(
	t *testing.T,
) {
	var output bytes.Buffer
	command, err := NewFencedCommandContext(
		context.Background(),
		5*time.Second,
		"/bin/sh",
		"-c",
		"printf configured",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	command.Stdout = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	command.Dir = "/path/changed/after/start"
	command.Stdout = nil
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("private command Wait error = %v", err)
	}
	if output.String() != "configured" {
		t.Fatalf("private command output = %q", output.String())
	}
	if command.ProcessState() == nil {
		t.Fatal("private guard was not reaped")
	}
}

func TestLinuxDurableReceiptPrivateDescriptorIsReservedAndCloseOnExec(
	t *testing.T,
) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	private := command.command.ExtraFiles[3]
	if private.Fd() < privateDurableReceiptMinFD {
		t.Fatalf(
			"private receipt descriptor = %d, want >= %d",
			private.Fd(),
			privateDurableReceiptMinFD,
		)
	}
	flags, err := unix.FcntlInt(private.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("atomic receipt duplicate lacks FD_CLOEXEC")
	}
}

func TestLinuxCallerUnlockDoesNotReleasePrivateReceiptLease(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(
		int(probe.Fd()),
		unix.LOCK_EX|unix.LOCK_NB,
	); err == nil {
		_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
		t.Fatal("caller LOCK_UN released the private receipt lease")
	}
}

func TestLinuxFailedPrivateLeaseDoesNotReleaseCallerLock(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if command != nil {
		command.Close()
	}
	if !errors.Is(err, ErrDurableReceiptAlreadyClaimed) {
		t.Fatalf("caller-held receipt setup error = %v", err)
	}
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(
		int(probe.Fd()),
		unix.LOCK_EX|unix.LOCK_NB,
	); err == nil {
		_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
		t.Fatal("failed library setup released the caller's receipt lock")
	}
}

func TestLinuxDurableReceiptRejectsStandardSourceDescriptor(t *testing.T) {
	saved, err := unix.Dup(0)
	if err != nil {
		t.Fatal(err)
	}
	source := os.NewFile(0, "standard-input")
	defer func() {
		_ = source.Close()
		_ = unix.Dup2(saved, 0)
		_ = unix.Close(saved)
	}()
	if err := validateDurableReceiptConfigPlatform(source); err == nil {
		t.Fatal("durable receipt source descriptor 0 was accepted")
	}
}

func TestLinuxDurableReceiptRejectsSourceCloseOnExecMutation(
	t *testing.T,
) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(
		file.Fd(),
		unix.F_SETFD,
		flags&^unix.FD_CLOEXEC,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); !errors.Is(
		err,
		ErrFencedCommandConfigurationChanged,
	) {
		t.Fatalf("source CLOEXEC mutation Start error = %v", err)
	}
	if command.command.Process != nil {
		t.Fatal("source CLOEXEC mutation spawned a guard")
	}
}

func TestLinuxDurableReceiptRejectsDuplicateCloseOnExecMutation(
	t *testing.T,
) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	duplicate := command.command.ExtraFiles[3]
	flags, err := unix.FcntlInt(duplicate.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(
		duplicate.Fd(),
		unix.F_SETFD,
		flags&^unix.FD_CLOEXEC,
	); err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); !errors.Is(
		err,
		ErrFencedCommandConfigurationChanged,
	) {
		t.Fatalf("duplicate CLOEXEC mutation Start error = %v", err)
	}
	if command.command.Process != nil {
		t.Fatal("duplicate CLOEXEC mutation spawned a guard")
	}
}

func TestLinuxDurableReceiptRejectsSourceWithoutCloseOnExec(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "receipt")
	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(
		file.Fd(),
		unix.F_SETFD,
		flags&^unix.FD_CLOEXEC,
	); err != nil {
		t.Fatal(err)
	}
	config, err := NewDurableReceiptConfig(file)
	if err == nil {
		if _, err := NewFencedCommandContextWithDurableReceipt(
			context.Background(),
			5*time.Second,
			config,
			"/bin/true",
		); err == nil {
			t.Fatal("receipt source without FD_CLOEXEC was accepted")
		}
	}
}

func TestLinuxDurableReceiptConfigCopiesAreOneShot(t *testing.T) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	copied := config
	first, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		copied,
		"/bin/true",
	); !errors.Is(err, ErrDurableReceiptAlreadyClaimed) {
		t.Fatalf("copied receipt config reuse error = %v", err)
	}
}

func TestLinuxDurableReceiptConfigMutationConsumesAllCopies(
	t *testing.T,
) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	original := config
	mutated := config
	mutated.ExecutionID = mutated.ReceiptID
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		mutated,
		"/bin/true",
	); err == nil {
		t.Fatal("mutated durable receipt config was accepted")
	}
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		original,
		"/bin/true",
	); !errors.Is(err, ErrDurableReceiptAlreadyClaimed) {
		t.Fatalf("original config after mutation attempt error = %v", err)
	}
}

func TestLinuxDurableReceiptFailedSetupConsumesConfig(t *testing.T) {
	file, _, config := openDurableReceipt(t)
	defer file.Close()
	if err := file.Chmod(0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	); err == nil {
		t.Fatal("insecure receipt setup unexpectedly succeeded")
	}
	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	); !errors.Is(err, ErrDurableReceiptAlreadyClaimed) {
		t.Fatalf("failed setup config reuse error = %v", err)
	}
}

func TestLinuxDurableReceiptSameInodeHasOneConcurrentOwner(
	t *testing.T,
) {
	firstFile, path, firstConfig := openDurableReceipt(t)
	defer firstFile.Close()
	secondFile, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer secondFile.Close()
	secondConfig, err := NewDurableReceiptConfig(secondFile)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		command *FencedCommand
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, config := range []DurableReceiptConfig{
		firstConfig,
		secondConfig,
	} {
		wait.Add(1)
		go func(config DurableReceiptConfig) {
			defer wait.Done()
			<-start
			command, err := NewFencedCommandContextWithDurableReceipt(
				context.Background(),
				5*time.Second,
				config,
				"/bin/true",
			)
			results <- result{command: command, err: err}
		}(config)
	}
	close(start)
	wait.Wait()
	close(results)

	winners := 0
	claimed := 0
	for current := range results {
		if current.err == nil {
			winners++
			current.command.Close()
			continue
		}
		if errors.Is(current.err, ErrDurableReceiptAlreadyClaimed) {
			claimed++
			continue
		}
		t.Fatalf("same-inode claim error = %v", current.err)
	}
	if winners != 1 || claimed != 1 {
		t.Fatalf(
			"same-inode outcomes: winners=%d claimed=%d",
			winners,
			claimed,
		)
	}
}

func TestLinuxDurableReceiptFlockBlocksOtherProcess(t *testing.T) {
	if os.Getenv(durableClaimHelper) == "1" {
		file, err := os.OpenFile(
			os.Getenv(durableFDReceiptPath),
			os.O_RDWR,
			0,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		config, err := NewDurableReceiptConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		command, err := NewFencedCommandContextWithDurableReceipt(
			context.Background(),
			5*time.Second,
			config,
			"/bin/true",
		)
		if command != nil {
			command.Close()
		}
		if !errors.Is(err, ErrDurableReceiptAlreadyClaimed) {
			t.Fatalf("cross-process flock error = %v", err)
		}
		return
	}

	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	competitor := exec.Command(
		executable,
		"-test.run=^TestLinuxDurableReceiptFlockBlocksOtherProcess$",
	)
	competitor.Env = append(
		os.Environ(),
		durableClaimHelper+"=1",
		durableFDReceiptPath+"="+path,
	)
	if output, err := competitor.CombinedOutput(); err != nil {
		t.Fatalf("cross-process claim helper: %v: %s", err, output)
	}
}

func TestLinuxDurableReceiptLeaseEndsAfterActualWait(t *testing.T) {
	file, path, config := openDurableReceipt(t)
	defer file.Close()
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(
		int(probe.Fd()),
		unix.LOCK_EX|unix.LOCK_NB,
	); err == nil {
		t.Fatal("receipt lease was not held before Start")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(
		int(probe.Fd()),
		unix.LOCK_EX|unix.LOCK_NB,
	); err != nil {
		t.Fatalf("receipt lease remained after actual Wait: %v", err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDurableReceiptCallerMayCloseOriginalBeforeStart(
	t *testing.T,
) {
	file, path, config := openDurableReceipt(t)
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		5*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	receipt := readDurableReceipt(t, path)
	if !receipt.Released || !receipt.Quiescent {
		t.Fatalf("closed-source receipt = %#v", receipt)
	}
}

func TestLinuxProcessStatIdentityIgnoresVolatileState(t *testing.T) {
	running := linuxProcessStat{
		state:          'R',
		parentPID:      10,
		processGroupID: 11,
		startTime:      12,
	}
	sleeping := running
	sleeping.state = 'S'
	if !running.sameProcessInstance(sleeping) {
		t.Fatal("R/S transition was treated as PID reuse")
	}
	reused := sleeping
	reused.startTime++
	if running.sameProcessInstance(reused) {
		t.Fatal("different process start time was accepted")
	}
}

func ExampleNewFencedCommandContextWithDurableReceipt() {
	directory, err := os.MkdirTemp("", "autogora-durable-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "receipt.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	config, err := NewDurableReceiptConfig(file)
	if err != nil {
		panic(err)
	}
	command, err := NewFencedCommandContextWithDurableReceipt(
		context.Background(),
		10*time.Second,
		config,
		"/bin/true",
	)
	if err != nil {
		panic(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		panic(err)
	}
	identity, err := command.Identity()
	if err != nil {
		panic(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		panic(fmt.Errorf("release: %t: %w", released, err))
	}
	if err := command.Wait(); err != nil {
		panic(err)
	}
	receipt, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	parsed, err := ParseDurableTeardownReceipt(receipt)
	if err != nil {
		panic(err)
	}
	fmt.Println(parsed.Identity == identity, parsed.Released, parsed.Quiescent)
	// Output: true true true
}
