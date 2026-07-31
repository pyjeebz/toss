// Command toss is the whole server: HTTP, SSE, and every room, in one process.
//
// CRITICAL -- RUN EXACTLY ONE INSTANCE.
//
// All state is in memory and there is no cross-process fan-out. Behind a load
// balancer with two instances, a POST landing on instance A while the
// recipient's SSE stream is held by instance B is delivered to nobody, logs no
// error, and returns 201 to the sender. The failure is completely silent, and
// it looks like "toss is flaky" rather than "toss is misconfigured".
//
// If this ever needs to scale horizontally, that is a real change -- a shared
// bus (Redis pub/sub, NATS) or sticky sessions keyed on room ID -- not a
// replica count bump.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pyjeebz/toss/internal/api"
	"github.com/pyjeebz/toss/internal/hub"
	"github.com/pyjeebz/toss/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := os.Getenv("TOSS_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	h := hub.New()
	srv := api.New(h, log)

	// Background collection: expired items, idle rooms, spent pairing codes and
	// rate-limit buckets. Stops with the server.
	sweepCtx, stopSweepers := context.WithCancel(context.Background())
	defer stopSweepers()
	h.StartSweeper(sweepCtx, log)
	srv.StartSweeper(sweepCtx)

	// Request contexts derive from this one, so cancelling it is how in-flight
	// SSE streams are told to wind down at shutdown. Without it, Shutdown
	// blocks on long-lived streams until its own deadline expires.
	baseCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(web.FS()),
		WriteTimeout:      0, // MUST be 0 -- any deadline cuts SSE streams mid-flight
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	stopSweepers()
	cancelStreams()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	log.Info("stopped")
}
