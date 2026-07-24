package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nn1a/autogora/internal/operatorrecovery"
	"github.com/nn1a/autogora/internal/store"
)

const quarantineConfirmationFileLimit = 1024 * 1024

const recoveryHelp = `autogora recovery <action> [options]

Actions:
  show <run-id>                  Show the public run-recovery fence
  confirm <run-id>               Confirm one fenced run is quiescent
  quarantine status             Show the global automation quarantine
  quarantine confirm --file <p> Resolve the exact quarantined source set

Run confirmation options:
  --fence-generation <n>        Exact generation shown by recovery show
  --actor <name>                 Operator identity recorded in the audit event
  --reason <text>                Reason and evidence for the confirmation
  --confirm-worker-stopped       Confirm the worker process tree stopped
  --confirm-host-writes-stopped  Confirm host and external workspace writes stopped

Quarantine confirmation reads a strict JSON file containing the exact global
generation and source set. It never accepts publication claim tokens. Stop the
Supervisor, helper processes, and external writers before confirming.

Neither confirmation command force-kills a process. The stored observations
and exact generations are checked again before recovery continues.
`

func (a *App) runRecovery(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("recovery requires show, confirm, or quarantine")
	}
	action := strings.ToLower(strings.TrimSpace(opts.positionals[0]))
	if action == "help" {
		_, err := a.Stdout.Write([]byte(recoveryHelp))
		return err
	}
	if action == "quarantine" {
		return a.runQuarantineRecovery(ctx, opts)
	}
	if len(opts.positionals) != 2 {
		return errors.New("recovery action requires exactly one run id")
	}
	runID := opts.positionals[1]
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	if _, err := opened.GetRun(ctx, runID); err != nil {
		return err
	}
	switch action {
	case "show":
		fence, err := opened.GetDeferredReclaim(ctx, runID)
		if err != nil {
			return err
		}
		if fence == nil {
			return errors.New("run has no active recovery fence")
		}
		return writeJSON(a.Stdout, fence)
	case "confirm":
		generation, err := numberOption(opts.value("fence-generation"), 0)
		if err != nil {
			return err
		}
		fence, err := opened.ConfirmRunRecoveryQuiescence(
			ctx,
			store.ConfirmRunRecoveryQuiescenceInput{
				RunID:                    runID,
				FenceGeneration:          generation,
				Actor:                    opts.value("actor"),
				Reason:                   opts.value("reason"),
				ConfirmWorkerStopped:     opts.flags["confirm-worker-stopped"],
				ConfirmHostWritesStopped: opts.flags["confirm-host-writes-stopped"],
			},
		)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, fence)
	default:
		return errors.New("recovery requires show or confirm")
	}
}

func readQuarantineConfirmationFile(
	path string,
) (operatorrecovery.Confirmation, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return operatorrecovery.Confirmation{}, errors.New(
			"recovery quarantine confirm requires --file",
		)
	}
	opened, err := os.Open(path)
	if err != nil {
		return operatorrecovery.Confirmation{}, fmt.Errorf(
			"open recovery confirmation file: %w",
			err,
		)
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return operatorrecovery.Confirmation{}, fmt.Errorf(
			"inspect recovery confirmation file: %w",
			err,
		)
	}
	if !info.Mode().IsRegular() {
		return operatorrecovery.Confirmation{}, errors.New(
			"recovery confirmation file must be a regular file",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		opened,
		quarantineConfirmationFileLimit+1,
	))
	if err != nil {
		return operatorrecovery.Confirmation{}, fmt.Errorf(
			"read recovery confirmation file: %w",
			err,
		)
	}
	if len(raw) > quarantineConfirmationFileLimit {
		return operatorrecovery.Confirmation{}, fmt.Errorf(
			"recovery confirmation file exceeds %d bytes",
			quarantineConfirmationFileLimit,
		)
	}
	return operatorrecovery.DecodeConfirmation(bytes.NewReader(raw))
}

func (a *App) runQuarantineRecovery(
	ctx context.Context,
	opts options,
) error {
	if opts.present("claim-token") || opts.present("claimToken") {
		return errors.New(
			"recovery quarantine commands do not accept claim tokens",
		)
	}
	if strings.TrimSpace(opts.value("board")) != "" {
		return errors.New(
			"recovery quarantine is global and does not accept --board",
		)
	}
	if len(opts.positionals) != 2 {
		return errors.New(
			"recovery quarantine requires status or confirm",
		)
	}
	action := strings.ToLower(strings.TrimSpace(opts.positionals[1]))
	manager, err := a.managerFor(opts.value("db"))
	if err != nil {
		return err
	}
	service, err := operatorrecovery.New(manager)
	if err != nil {
		return err
	}
	switch action {
	case "status":
		if opts.present("file") {
			return errors.New(
				"recovery quarantine status does not accept --file",
			)
		}
		value, err := service.Status(ctx)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	case "confirm":
		input, err := readQuarantineConfirmationFile(opts.value("file"))
		if err != nil {
			return err
		}
		value, err := service.Confirm(ctx, input)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	default:
		return errors.New(
			"recovery quarantine requires status or confirm",
		)
	}
}
