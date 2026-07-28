// Command clirelay-updater is the sidecar that applies Docker deployment updates on
// behalf of the CliRelay management API.
//
// It is intentionally ignorant of the application it updates. It receives a
// declarative plan naming an image and a list of compose services, executes it, and
// reports progress. It does not read, interpret or rewrite the deployment's compose
// file. Everything the application needs migrated is migrated by the application's
// own preparation container, which the plan names.
//
// That ignorance is the point: this process must keep working across refactors of
// the thing it updates, including refactors that change the deployment's shape.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"clirelay.local/updater/server"
)

const (
	defaultListenAddr    = ":8320"
	defaultTargetService = "clirelay"
	shutdownTimeout      = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	addr := envOrDefault("CLIRELAY_UPDATER_ADDR", defaultListenAddr)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("clirelay updater: listen on %s failed: %w", addr, err)
	}
	return serve(ctx, configFromEnv(), listener)
}

func configFromEnv() server.Config {
	return server.Config{
		Token:          strings.TrimSpace(os.Getenv("CLIRELAY_UPDATER_TOKEN")),
		ComposeFile:    strings.TrimSpace(os.Getenv("CLIRELAY_COMPOSE_FILE")),
		EnvFile:        strings.TrimSpace(os.Getenv("CLIRELAY_ENV_FILE")),
		ProjectName:    strings.TrimSpace(os.Getenv("CLIRELAY_COMPOSE_PROJECT_NAME")),
		DefaultService: envOrDefault("CLIRELAY_TARGET_SERVICE", defaultTargetService),
		StateFile:      strings.TrimSpace(os.Getenv("CLIRELAY_UPDATER_STATE_FILE")),
	}
}

func serve(ctx context.Context, config server.Config, listener net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	updater := server.New(ctx, config)

	httpServer := &http.Server{
		Handler:           updater.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: /v1/events is a long-lived stream and a write deadline
		// would sever it mid-update.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("clirelay updater listening on %s", listener.Addr())
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("clirelay updater: shutdown failed: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func envOrDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
