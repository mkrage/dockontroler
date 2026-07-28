// Command dockontroler serves a small web UI, and optionally a Telegram bot, for
// starting, stopping and reconfiguring the Docker containers on one host.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mkrage/dockontroler/internal/config"
	"github.com/mkrage/dockontroler/internal/docker"
	"github.com/mkrage/dockontroler/internal/manager"
	"github.com/mkrage/dockontroler/internal/telegram"
	"github.com/mkrage/dockontroler/internal/web"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// Timeouts for the HTTP server.
//
// writeTimeout has to clear the longest request a handler can legitimately take,
// which is a recreate: stop the old container, build and start a replacement.
// Anything shorter would cut the response off mid-operation.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 5 * time.Minute
	idleTimeout       = 90 * time.Second
	shutdownGrace     = 30 * time.Second
	dockerPingTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dockontroler: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	logger.Info("dockontroler starting", "version", version)

	// Cancelled on Ctrl-C or on the SIGTERM that `docker stop` sends.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	client := docker.New(cfg.DockerSocket)

	// Fail fast on an unreachable socket. Starting up and only failing when
	// somebody opens the page would make a mounting mistake far harder to spot.
	pingCtx, cancelPing := context.WithTimeout(ctx, dockerPingTimeout)
	ping, err := client.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("cannot talk to the docker daemon at %s: %w\n"+
			"is the socket mounted? try: -v %s:%s",
			cfg.DockerSocket, err, cfg.DockerSocket, cfg.DockerSocket)
	}
	logger.Info("connected to docker",
		"socket", cfg.DockerSocket, "api_version", ping.APIVersion, "os", ping.OSType)

	selfID := manager.DetectSelfID(ctx, client, cfg.SelfContainerID)
	switch {
	case selfID != "":
		logger.Info("self-protection active", "self_id", selfID[:12])
	case cfg.SelfContainerID != "":
		logger.Warn("DOCKONTROLER_SELF_ID does not match any container, so dockontroler "+
			"cannot protect itself from being stopped through its own UI",
			"configured", cfg.SelfContainerID)
	default:
		logger.Warn("could not determine dockontroler's own container id, so it cannot " +
			"protect itself from being stopped through its own UI; " +
			"set DOCKONTROLER_SELF_ID to silence this")
	}

	containers := manager.New(client, logger, cfg.StopTimeout, selfID)

	webServer, err := web.New(containers, logger, cfg.RefreshInterval)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           webServer.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	var (
		workers sync.WaitGroup
		fatal   = make(chan error, 1)
	)

	workers.Add(1)
	go func() {
		defer workers.Done()
		logger.Info("web ui listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Losing the listener means the tool is useless, so this one is fatal.
			fatal <- fmt.Errorf("web server: %w", err)
		}
	}()

	if cfg.BotEnabled() {
		bot := telegram.New(cfg.TelegramToken, cfg.TelegramChatIDs, containers, logger)
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := bot.Run(ctx); err != nil {
				// Deliberately not fatal: a rejected token or a Telegram outage
				// should not take the web UI down with it.
				logger.Error("telegram bot unavailable, continuing without it", "error", err)
			}
		}()
	} else {
		logger.Info("telegram bot disabled (TELEGRAM_BOT_TOKEN is not set)")
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case runErr = <-fatal:
		// Stop the bot too, then fall through to the orderly shutdown below.
		stopSignals()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Most likely an operation still in flight. A recreate detaches its own
		// context, but the process exiting still cuts it short — which can leave
		// a container behind under its "__dockontroler_old" name.
		logger.Warn("web server did not shut down cleanly", "error", err)
	}

	workers.Wait()
	logger.Info("dockontroler stopped")
	return runErr
}
