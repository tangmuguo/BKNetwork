package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaticFilesDisableBrowserCache(t *testing.T) {
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("v1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	noStoreFileServer(webDir).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("static response status = %d; want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "v1.0.0") {
		t.Fatalf("unexpected static response: %q", recorder.Body.String())
	}
}

func TestServerReadyAfterOwningListener(t *testing.T) {
	srv := NewServer("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case <-srv.Ready():
	case err := <-errCh:
		t.Fatalf("server exited before becoming ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not signal readiness")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server shutdown returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerDoesNotSignalReadyWhenAddressIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	srv := NewServer(listener.Addr().String())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(context.Background()) }()

	select {
	case <-srv.Ready():
		t.Fatal("server reported ready even though another listener owns the address")
	case startErr := <-errCh:
		if startErr == nil || !strings.Contains(startErr.Error(), "http listen error") {
			t.Fatalf("unexpected listen error: %v", startErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not report the listen conflict")
	}
}
