//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bknetwork/internal/server"
)

func TestProbeV7UIUsesStableBackendMarker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(server.ReadyPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.ReadyHeader, server.ReadyMarker)
		w.WriteHeader(http.StatusOK)
	})
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	if err := probeV7UI(testServer.Client(), testServer.URL); err != nil {
		t.Fatalf("probeV7UI returned an error: %v", err)
	}
}

func TestProbeV7UIRejectsHTMLVersionTextWithoutBackendMarker(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>BKNetwork v1.0.0</html>`))
	}))
	defer testServer.Close()

	err := probeV7UI(testServer.Client(), testServer.URL)
	if err == nil {
		t.Fatal("probeV7UI accepted an HTML version string without the backend marker")
	}
	if !strings.Contains(err.Error(), "readiness marker") {
		t.Fatalf("unexpected error: %v", err)
	}
}
