//go:build !windows

package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"bknetwork/internal/server"

	"github.com/kardianos/service"
)

func isServiceProcess() (bool, error) {
	return !service.Interactive(), nil
}

func openRelaunchedDesktopUI() error {
	return nil
}

func reportDesktopFailure(err error) {
	if err != nil {
		log.Printf("desktop startup failed: %v", err)
	}
}

func runDesktopApp() error {
	srv := server.NewServer("")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		_ = srv.Start(ctx)
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
