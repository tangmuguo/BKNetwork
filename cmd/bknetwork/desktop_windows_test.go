//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bknetwork/internal/server"
)

func TestProbeUIUsesStableBackendMarker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(server.ReadyPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.ReadyHeader, server.ReadyMarker)
		w.WriteHeader(http.StatusOK)
	})
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	if err := probeUI(testServer.Client(), testServer.URL); err != nil {
		t.Fatalf("probeUI returned an error: %v", err)
	}
}

func TestProbeUIRejectsHTMLTextWithoutBackendMarker(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>legacy BKNetwork UI</html>`))
	}))
	defer testServer.Close()

	err := probeUI(testServer.Client(), testServer.URL)
	if err == nil {
		t.Fatal("probeUI accepted HTML without the backend marker")
	}
	if !strings.Contains(err.Error(), "readiness marker") {
		t.Fatalf("unexpected error: %v", err)
	}
}
