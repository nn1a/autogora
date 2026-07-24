package processguard

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// FencedCommand holds a guard process behind a one-shot start fence. Command
// is the durable process: the guarded target does not start until Release.
type FencedCommand struct {
	Command *exec.Cmd
	reader  *os.File
	writer  *os.File
	proof   teardownProof
	report  func(error)
}

func (c *FencedCommand) Start() error {
	if err := c.Command.Start(); err != nil {
		c.proof.close()
		return err
	}
	if err := c.proof.afterStart(); err != nil {
		_ = c.Command.Process.Kill()
		c.proof.close()
		err = errors.Join(ErrTeardownUnconfirmed, err)
		c.reportUnconfirmed(err)
		return err
	}
	return nil
}

func (c *FencedCommand) Wait() error {
	err := errors.Join(c.Command.Wait(), c.proof.confirm())
	c.reportUnconfirmed(err)
	return err
}

func (c *FencedCommand) Release() (bool, error) {
	written, writeErr := c.writer.Write([]byte{'\n'})
	closeWriterErr := c.writer.Close()
	closeReaderErr := c.reader.Close()
	if written == 1 {
		return true, nil
	}
	return false, errors.Join(writeErr, closeWriterErr, closeReaderErr, io.ErrShortWrite)
}

func (c *FencedCommand) Close() {
	_ = c.reader.Close()
	_ = c.writer.Close()
	c.proof.close()
}

func (c *FencedCommand) reportUnconfirmed(err error) {
	if errors.Is(err, ErrTeardownUnconfirmed) && c.report != nil {
		c.report(err)
	}
}

func newFencedCommand(
	ctx context.Context,
	command *exec.Cmd,
	reader, writer *os.File,
	proof teardownProof,
) *FencedCommand {
	return &FencedCommand{
		Command: command,
		reader:  reader,
		writer:  writer,
		proof:   proof,
		report:  teardownFailureReporter(ctx),
	}
}
