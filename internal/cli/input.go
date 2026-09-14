package cli

import (
	"bytes"
	"context"
	"io"
	"os"

	"golang.org/x/term"
)

// Inherited terminal descriptors can block even when closed from another
// goroutine. Let the command stop without waiting for that read. The worker
// owns its buffer and never changes terminal state or writes command output.
type contextInputReader struct {
	ctx       context.Context
	reader    io.Reader
	interrupt bool
}

func (r *contextInputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	type result struct {
		data []byte
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		buffer := make([]byte, len(p))
		n, err := r.reader.Read(buffer)
		ready <- result{buffer[:n], err}
	}()
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case read := <-ready:
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		// Raw terminals deliver Ctrl-C as a byte instead of SIGINT.
		if r.interrupt && bytes.IndexByte(read.data, 3) >= 0 {
			return 0, context.Canceled
		}
		return copy(p, read.data), read.err
	}
}

func (a *App) readPassword(file *os.File) ([]byte, error) {
	if err := a.commandContext().Err(); err != nil {
		return nil, err
	}
	// Change and restore terminal state on the calling goroutine so a late
	// input read cannot disable echo again after cancellation has restored it.
	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	defer term.Restore(int(file.Fd()), state)
	input := &contextInputReader{ctx: a.commandContext(), reader: file, interrupt: true}
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{input, io.Discard}, "")
	password, err := terminal.ReadPassword("")
	return []byte(password), err
}
