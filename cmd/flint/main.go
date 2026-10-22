package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/flint-pay/flint-cli/internal/cli"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

var (
	version    = "dev"
	commit     = "unknown"
	buildDate  = "unknown"
	schemaHash = "development"
)

// duplicateSignalWindow is how long further signals are ignored after the
// first one. A single Ctrl-C can arrive more than once within milliseconds:
// the terminal signals the whole foreground process group, and the npm wrapper
// also forwards its own copy. A deliberate second signal after the window
// still stops a command that is stuck during cleanup.
var duplicateSignalWindow = time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) (exitCode int) {
	exitCode = cli.ExitSoftware
	defer func() {
		if recover() != nil {
			fmt.Fprintf(os.Stderr, "flint encountered an internal error (version %s). No sensitive diagnostic data was printed.\n", version)
			exitCode = cli.ExitSoftware
		}
	}()
	ctx, stop := signalContext(context.Background())
	defer stop()
	app := cli.New(cli.BuildInfo{
		Version:    version,
		Commit:     commit,
		BuildDate:  buildDate,
		APIVersion: apispec.APIVersion(),
		SchemaHash: schemaHash,
	})
	app.Context = ctx
	return app.Run(argv)
}

// signalContext returns a context canceled by the first interrupt or
// termination signal, which requests graceful cancellation. Duplicate signals
// within duplicateSignalWindow are discarded; afterwards default handling is
// restored, so another signal terminates the process.
func signalContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
		case <-done:
			signal.Stop(signals)
			return
		}
		cancel()
		// Keep discarding signals for the whole window, even after stop: a
		// command that cancels quickly must not be killed by the duplicate
		// while the process exits.
		window := time.NewTimer(duplicateSignalWindow)
		defer window.Stop()
		for {
			select {
			case <-signals:
			case <-window.C:
				signal.Stop(signals)
				return
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(done)
			cancel()
		})
	}
}
