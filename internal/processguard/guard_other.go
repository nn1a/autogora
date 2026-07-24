//go:build !linux

package processguard

import (
	"context"
	"os"
	"os/exec"
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
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	shellArgs := []string{
		"-c",
		`IFS= read -r autogora_start <&3 || exit 125; exec "$@"`,
		"autogora-start-barrier",
		name,
	}
	shellArgs = append(shellArgs, args...)
	command := exec.Command("/bin/sh", shellArgs...)
	command.ExtraFiles = []*os.File{reader}
	return newFencedCommand(
		ctx,
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
