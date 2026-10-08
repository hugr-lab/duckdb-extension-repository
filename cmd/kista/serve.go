package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hugr-lab/duckdb-extension-repository/internal/app"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

// serveCmd runs `kista serve` until SIGTERM or SIGINT.
func serveCmd(args, env []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("kista serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file")
	if err := fs.Parse(args); err != nil || *cfgPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: kista serve -config <file>")
		return 2
	}
	cfg, err := config.Load(*cfgPath, env)
	if err != nil {
		fmt.Fprintln(stderr, "kista serve:", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if cfg.Profile == config.ProfileDev {
		log.Warn("kista serve: profile dev")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := app.Serve(ctx, cfg, log); err != nil {
		fmt.Fprintln(stderr, "kista serve:", err)
		return 1
	}
	return 0
}
