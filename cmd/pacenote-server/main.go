// Command pacenote-server is the Pacenote community edition server: one binary an
// operator points at a PostgreSQL database and configures in a browser.
//
// Run it with no arguments. On a first run it prints a setup token and serves
// the wizard; after that it serves the admin panel and, in a later release, the
// telemetry API.
//
//	./pacenote-server
//	./pacenote-server -data ./somewhere-else
//
// Everything the wizard collects can also come from the environment, which is
// what a container deployment uses:
//
//	PACENOTE_DATA_DIR         where config.json lives (default: beside the binary)
//	PACENOTE_DATABASE_URL     the PostgreSQL connection string
//	PACENOTE_LISTEN           the public address (default: :8080)
//	PACENOTE_METRICS_LISTEN   the private address (default: 127.0.0.1:9090)
//	PACENOTE_SECRET_KEY       the data key that opens stored API keys
//	PACENOTE_LOG_LEVEL        debug, info, warn or error (default: info)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/pacenote-sim/server/internal/app"
	"github.com/pacenote-sim/server/internal/buildinfo"
	"github.com/pacenote-sim/server/internal/config"
	"github.com/pacenote-sim/server/internal/logging"
)

// EnvLogLevel names the variable that sets how much is logged.
const EnvLogLevel = "PACENOTE_LOG_LEVEL"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir     = flag.String("data", "", "data directory (default: "+config.DirName+" beside the binary)")
		logLevel    = flag.String("log-level", "", "debug, info, warn or error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	info := buildinfo.Read()
	if *showVersion {
		fmt.Println("pacenote-server " + info.Long())
		return nil
	}

	level, err := parseLevel(firstNonEmpty(*logLevel, os.Getenv(EnvLogLevel)))
	if err != nil {
		return err
	}
	log := logging.New(logging.Options{Level: level, Output: os.Stdout})

	// SIGINT and SIGTERM both mean stop: the first is a person at a terminal,
	// the second is an init system or a container runtime. Both get the same
	// graceful shutdown, and a second one kills the process the usual way
	// because signal.NotifyContext stops intercepting after the first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, app.Options{
		DataDir:  *dataDir,
		Log:      log,
		Terminal: os.Stdout,
		Version:  info.Short(),
	})
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%q is not a log level — use debug, info, warn or error", s)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
