package terminalui

import (
	"context"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

type Options struct {
	Board  string
	Input  io.Reader
	Output io.Writer
}

func Run(ctx context.Context, backend Backend, options Options) error {
	input, output := options.Input, options.Output
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stdout
	}
	program := tea.NewProgram(
		NewModel(ctx, backend, options.Board),
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(output),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := program.Run()
	return err
}
