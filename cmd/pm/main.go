package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/cli"
)

var version = "devel"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := cli.Execute(ctx, cli.Options{
		Args:      os.Args[1:],
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		LookupEnv: os.LookupEnv,
		Version:   version,
	})
	os.Exit(code)
}
