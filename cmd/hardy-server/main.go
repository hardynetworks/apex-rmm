// Command hardy-server runs the Hardy RMM API, dashboard and agent hub.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
	"github.com/hardynetworks/hardy-rmm/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("starting Hardy RMM server", "version", proto.Version)
	cfg, err := server.LoadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := server.New(ctx, cfg)
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
