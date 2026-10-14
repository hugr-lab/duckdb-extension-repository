package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/hugr-lab/duckdb-extension-repository/internal/app"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

// serveCmd runs `kista serve` until SIGTERM or SIGINT.
func serveCmd(args, env []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("kista serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file, or env:NAME for a variable holding it")
	if err := fs.Parse(args); err != nil || *cfgPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: kista serve -config <file|env:NAME>")
		return 2
	}
	cfg, err := config.Load(*cfgPath, env)
	if err != nil {
		fmt.Fprintln(stderr, "kista serve:", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Info("kista serve: starting", "version", app.Version, "go", runtime.Version(), "platform", runtime.GOOS+"/"+runtime.GOARCH)
	if cfg.Profile == config.ProfileDev {
		log.Warn("kista serve: profile dev")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second signal ends the process at once, not after the drain
	if err := app.Serve(ctx, cfg, log); err != nil {
		fmt.Fprintln(stderr, "kista serve:", err)
		return 1
	}
	return 0
}
