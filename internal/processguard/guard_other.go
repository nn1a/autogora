//go:build !linux

package processguard

import (
	"context"
	"os"
	"os/exec"
	"time"
)

type directTeardownProof struct{}

func (directTeardownProof) afterStart() error { return nil }
func (directTeardownProof) confirm() error    { return nil }
func (directTeardownProof) close()            {}

// NewFencedCommand retains the pre-existing pipe fence on platforms without a
// subreaper. Callers must keep conservative recovery/operator policy there.
func NewFencedCommand(
	ctx context.Context,
	name string,
	args ...string,
) (*FencedCommand, error) {
	return NewFencedCommandContext(ctx, 0, name, args...)
}

// NewFencedCommandContext keeps the start barrier context-bound on fallback
// platforms. These platforms still cannot attest descendant teardown, so
// automatic mutations remain disabled by the capability boundary.
func NewFencedCommandContext(
	ctx context.Context,
	maximum time.Duration,
	name string,
	args ...string,
) (*FencedCommand, error) {
	bounded, cancel := boundedContext(ctx, maximum)
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	shellArgs := []string{
		"-c",
		`IFS= read -r autogora_start <&3 || exit 125; exec "$@"`,
		"autogora-start-barrier",
		name,
	}
	shellArgs = append(shellArgs, args...)
	command := exec.CommandContext(bounded, "/bin/sh", shellArgs...)
	command.ExtraFiles = []*os.File{reader}
	return newFencedCommand(
		ctx,
		bounded,
		cancel,
		command,
		reader,
		writer,
		directTeardownProof{},
	), nil
}

func newGuardedCommandContext(
	ctx context.Context,
	name string,
	args ...string,
) (*exec.Cmd, teardownProof) {
	return exec.CommandContext(ctx, name, args...), directTeardownProof{}
}

func IsGuardCommand(_ *exec.Cmd) bool { return false }

// Direct fallback commands cannot attest descendants. Managed recovery must
// remain operator-only on these platforms.
func TeardownProofAvailable() bool { return false }
