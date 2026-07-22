package cli

import (
	"context"

	"github.com/nn1a/autogora/internal/terminalui"
)

func (a *App) runTUI(ctx context.Context, opts options) error {
	opened, _, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()

	return terminalui.Run(ctx, opened, terminalui.Options{
		Board:  board,
		Input:  a.Stdin,
		Output: a.Stdout,
	})
}
