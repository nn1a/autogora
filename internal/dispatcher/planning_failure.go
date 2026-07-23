package dispatcher

import (
	"context"
	"time"

	"github.com/nn1a/autogora/internal/store"
)

// failAutoDecomposeClaim records every failure after an attempt is charged,
// including local Planner construction failures before an external process
// starts. It returns true when the parent dispatch context is already done.
func failAutoDecomposeClaim(
	ctx context.Context,
	opened *store.Store,
	claim store.AutoDecomposeClaim,
	cause error,
	options Options,
) bool {
	persistCtx := ctx
	cancelPersist := func() {}
	if ctx.Err() != nil {
		persistCtx, cancelPersist = context.WithTimeout(context.Background(), 2*time.Second)
	}
	failure, failureErr := opened.FailAutoDecompose(
		persistCtx,
		claim,
		cause.Error(),
		options.currentTime(),
	)
	cancelPersist()
	switch {
	case failureErr != nil:
		options.log(
			"auto-decompose failure persistence failed %s: %v",
			claim.TaskID,
			failureErr,
		)
	case failure.Eligibility == store.AutoDecomposeInvalidated:
		options.log(
			"auto-decompose result ignored %s because the Triage task changed",
			claim.TaskID,
		)
	case failure.Eligibility == store.AutoDecomposeExhausted:
		options.log(
			"auto-decompose exhausted %s after %d/%d attempts: %v",
			claim.TaskID,
			failure.Attempt,
			failure.MaxAttempts,
			cause,
		)
	case failure.RetryAt != nil:
		options.log(
			"auto-decompose failed %s (attempt %d/%d, retry after %s): %v",
			claim.TaskID,
			failure.Attempt,
			failure.MaxAttempts,
			*failure.RetryAt,
			cause,
		)
	default:
		options.log("auto-decompose failed %s: %v", claim.TaskID, cause)
	}
	return ctx.Err() != nil
}
