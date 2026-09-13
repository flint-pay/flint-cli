package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/flint-pay/flint-cli/internal/cli"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

var (
	version    = "dev"
	commit     = "unknown"
	buildDate  = "unknown"
	schemaHash = "development"
)

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
