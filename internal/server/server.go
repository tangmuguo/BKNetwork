package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"bknetwork/internal/events"
	"bknetwork/internal/handlers"
)

type Server struct {
	httpServer *http.Server
	hub        *events.Hub
	ready      chan struct{}
	readyOnce  sync.Once
}

const DefaultAddr = "127.0.0.1:13335"

func NewServer(addr string) *Server {
	if addr == "" {
		addr = DefaultAddr
	}
	mux := http.NewServeMux()
	hub := events.NewHub()
	mux.HandleFunc("/api/v1/switch", handlers.SwitchStackHandler(hub))
	mux.HandleFunc("/api/v1/dns", handlers.DnsHandler(hub))
	mux.HandleFunc("/api/v1/warp", handlers.WarpHandler(hub))
	mux.HandleFunc("/api/v1/warp-mode", handlers.WarpModeHandler(hub))
	mux.HandleFunc("/api/v1/warp-status", handlers.WarpStatusHandler())
	mux.HandleFunc("/api/v1/chatgpt-proxy", handlers.ChatGPTProxyHandler(hub))
	mux.HandleFunc("/api/v1/chatgpt-proxy.pac", handlers.ChatGPTProxyPACHandler())
	mux.HandleFunc("/api/v1/settings", handlers.SettingsHandler(hub))
	mux.HandleFunc("/api/v1/status", handlers.StatusHandler(hub))
	mux.HandleFunc("/api/v1/version/latest", handlers.LatestVersionHandler())
	mux.HandleFunc("/ws", handlers.WSHandler(hub))

	// static files: prefer the executable directory, then the current working directory.
	if webDir, ok := resolveWebDir(); ok {
		mux.Handle("/", noStoreFileServer(webDir))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}

	return &Server{
		hub:   hub,
		ready: make(chan struct{}),
		httpServer: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

func noStoreFileServer(webDir string) http.Handler {
	staticFiles := http.FileServer(http.Dir(webDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI is served from a fixed localhost URL across upgrades. Prevent
		// an old index/app.js pair from surviving when users replace v6 with v7.
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		staticFiles.ServeHTTP(w, r)
	})
}

func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

func resolveWebDir() (string, bool) {
	if exePath, err := os.Executable(); err == nil {
		if dir := filepath.Join(filepath.Dir(exePath), "web"); isDir(dir) {
			return dir, true
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if dir := filepath.Join(cwd, "web"); isDir(dir) {
			return dir, true
		}
	}
	return "", false
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("http listen error: %w", err)
	}
	s.readyOnce.Do(func() { close(s.ready) })

	lnErr := make(chan error, 1)
	go func() {
		log.Printf("Starting HTTP server on %s\n", s.httpServer.Addr)
		lnErr <- s.httpServer.Serve(listener)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	case err := <-lnErr:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("http listen error: %w", err)
		}
	case <-sig:
		return s.Shutdown(context.Background())
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("Shutting down server...")
	return s.httpServer.Shutdown(ctx)
}
