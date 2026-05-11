package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"inputd/internal/config"
	"inputd/internal/daemon"
)

func main() {
	cfgPath  := flag.String("config",    "/etc/inputd/config.yaml", "path to config file")
	webAddr  := flag.String("web-addr",  "0.0.0.0:17888",          "web UI listen address (empty to disable)")
	logLevel := flag.String("log-level", "info",                    "log level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{} // journald provides the timestamp
			}
			return a
		},
	})))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("config not found, writing default", "path", *cfgPath)
			cfg = config.Default()
			if mkErr := os.MkdirAll("/etc/inputd", 0755); mkErr == nil {
				config.Save(*cfgPath, cfg) //nolint:errcheck
			}
		} else {
			slog.Error("failed to load config", "err", err)
			os.Exit(1)
		}
	}

	d := daemon.New(cfg, *cfgPath, *webAddr)
	if err := d.Start(); err != nil {
		slog.Error("daemon start failed", "err", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for s := range sig {
		switch s {
		case syscall.SIGHUP:
			slog.Info("SIGHUP received, reloading config")
			if err := d.Reload(); err != nil {
				slog.Error("reload failed", "err", err)
			}
		default:
			slog.Info("signal received, shutting down", "signal", s)
			d.Stop()
			return
		}
	}
}
