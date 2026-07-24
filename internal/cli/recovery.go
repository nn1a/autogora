package cli

import (
	"context"
	"errors"

	"github.com/nn1a/autogora/internal/store"
)

const recoveryHelp = `autogora recovery <action> [options]

Actions:
  show <run-id>       Show the public recovery fence and exact generation
  confirm <run-id>    Confirm that an operator verified quiescence

Confirm options:
  --fence-generation <n>        Exact generation shown by recovery show
  --actor <name>                 Operator identity recorded in the audit event
  --reason <text>                Reason and evidence for the confirmation
  --confirm-worker-stopped       Confirm the worker process tree stopped
  --confirm-host-writes-stopped  Confirm host and external workspace writes stopped

Confirmation does not force-kill a process. The Supervisor rechecks the stored
run observation before it may inspect the workspace or continue recovery.
`

func (a *App) runRecovery(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("recovery requires show or confirm")
	}
	action := opts.positionals[0]
	if action == "help" {
		_, err := a.Stdout.Write([]byte(recoveryHelp))
		return err
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
