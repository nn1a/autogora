//go:build windows

package processguard

import "os/exec"

func configurePrivateFencedCommand(_ *exec.Cmd) {}

func resumePrivateFencedCommand(_ *exec.Cmd) error { return nil }
