// Command slayground is a lightweight idle-suspending HTTP proxy for Docker
// Compose stacks. It forwards traffic to one or more upstream containers and,
// when no meaningful traffic has been seen for a while, stops the other
// containers in its Compose project. The next request shows a "please wait"
// page, starts the containers again, and resumes forwarding once they are
// healthy.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jclement/slayground/internal/config"
	"github.com/jclement/slayground/internal/docker"
	"github.com/jclement/slayground/internal/proxy"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	logger.Info("slayground starting",
		"version", version,
		"listen", cfg.Listen,
		"idle_timeout", cfg.IdleTimeout,
	)

	srv, err := proxy.New(cfg, buildController(cfg, logger), logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go srv.Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// buildController wires up Docker-based suspend/resume. It returns nil (plain
// proxy mode, no suspend/resume) when Docker is unreachable or the Compose
// project cannot be determined.
func buildController(cfg *config.Config, logger *slog.Logger) proxy.StackController {
	client := docker.NewClient(cfg.DockerSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		logger.Warn("docker is unreachable; running as a plain proxy without suspend/resume",
			"socket", cfg.DockerSocket, "error", err)
		return nil
	}

	project := cfg.ComposeProject
	selfID := ""
	if p, id, err := client.DiscoverSelf(ctx); err == nil {
		if project == "" {
			project = p
		}
		selfID = id
	} else if project == "" {
		logger.Warn("could not discover compose project; running as a plain proxy without suspend/resume",
			"error", err,
			"hint", "set SLAYGROUND_COMPOSE_PROJECT to manage a project explicitly")
		return nil
	}

	logger.Info("managing compose project", "project", project, "self", shortID(selfID))
	return &docker.Manager{
		Client:         client,
		Project:        project,
		SelfID:         selfID,
		StopTimeout:    cfg.StopTimeout,
		StartupTimeout: cfg.StartupTimeout,
		Log:            logger,
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
