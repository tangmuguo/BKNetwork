package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bknetwork/internal/appinfo"
)

func TestStaticFilesDisableBrowserCache(t *testing.T) {
	handler, _ := platformWebHandler()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("static response status = %d; want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "BKNetwork") {
		t.Fatalf("unexpected static response: %q", recorder.Body.String())
	}
}

func TestReadyProbeHasStableBackendMarker(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, ReadyPath, nil)

	readyHandler(true).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("ready response status = %d; want 200", recorder.Code)
	}
	if got := recorder.Header().Get(ReadyHeader); got != ReadyMarker {
		t.Fatalf("%s = %q; want %q", ReadyHeader, got, ReadyMarker)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"version":"`+appinfo.Version+`"`) {
		t.Fatalf("ready response does not report version %s: %q", appinfo.Version, body)
	}
}

func TestReadyProbeRejectsMissingWebUI(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, ReadyPath, nil)

	readyHandler(false).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready response status = %d; want 503", recorder.Code)
	}
	if got := recorder.Header().Get(ReadyHeader); got != "" {
		t.Fatalf("%s = %q; want no ready marker", ReadyHeader, got)
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
