package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"

	"github.com/joho/godotenv"

	"divisor/internal/config"
	"divisor/internal/events"
	"divisor/internal/network"
)

var logLvels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

var opts struct {
	logger struct {
		level string
	}
}

func main() {
	flag.StringVar(&opts.logger.level, "log-level", "debug", "Set the log level")
	flag.Parse()

	logLevel, ok := logLvels[opts.logger.level]
	if !ok {
		log.Fatalf("Invalid log level: %s", opts.logger.level)
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(handler))

	if err := godotenv.Load(".env"); err != nil {
		log.Fatalf("Error loading .env file: %s", err)
	}

	cfg := config.Load()
	if err := cfg.ValidateMode(); err != nil {
		log.Fatalf("%s", err)
	}
	if err := cfg.ValidateBackend(); err != nil {
		log.Fatalf("%s", err)
	}

	// In direct mode nothing else configures the interface, so do it on startup.
	if cfg.Mode == config.ModeDirect {
		// New addresses are added before the old ones are removed, so
		// without promote_secondaries we'd lose the new address as well.
		if err := network.DirectModeReady(cfg.InterfaceName); err != nil {
			log.Fatalf("%s", err)
		}
		if err := network.Reconfigure(); err != nil {
			log.Fatalf("Initial network configuration failed: %s", err)
		}
	}

	slog.Info("Starting the Divisor...")
	source, err := events.Open(cfg)
	if err != nil {
		log.Fatalf("%s", err)
	}
	for {
		if err := source.Listen(context.Background(), network.Reconfigure); err != nil {
			slog.Error("main loop errored out", "error", err)
		}
	}
}
