// Command apex-server runs the Apex RMM API, dashboard and agent hub.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"github.com/hardynetworks/apex-rmm/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	// "apex-server init-secrets <dir>" generates the DB password and app key (docker compose "init" service).
	if len(os.Args) > 2 && os.Args[1] == "init-secrets" {
		if err := server.InitSecrets(os.Args[2]); err != nil {
			slog.Error("init-secrets", "err", err)
			os.Exit(1)
		}
		return
	}
	slog.Info("starting Apex RMM server", "version", proto.Version)
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
