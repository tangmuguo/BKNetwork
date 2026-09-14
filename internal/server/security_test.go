package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalAPIBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, host, origin, site, content, remote string
		want                                      int
	}{
		{"browser", "127.0.0.1:13335", "http://127.0.0.1:13335", "same-origin", "application/json", "127.0.0.1:6000", 200},
		{"cli", "localhost:13335", "", "", "application/json", "127.0.0.1:6000", 200},
		{"rebinding", "evil.example:13335", "", "", "application/json", "127.0.0.1:6000", 403},
		{"wrong port", "127.0.0.1:13336", "", "", "application/json", "127.0.0.1:6000", 403},
		{"foreign origin", "127.0.0.1:13335", "https://evil.example", "", "application/json", "127.0.0.1:6000", 403},
		{"null origin", "127.0.0.1:13335", "null", "", "application/json", "127.0.0.1:6000", 403},
		{"cross site", "127.0.0.1:13335", "", "cross-site", "application/json", "127.0.0.1:6000", 403},
		{"form", "127.0.0.1:13335", "", "", "text/plain", "127.0.0.1:6000", 415},
		{"remote client", "127.0.0.1:13335", "", "", "application/json", "192.0.2.1:6000", 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }), DefaultAddr)
			r := httptest.NewRequest("POST", "http://"+tt.host+"/api/v1/disconnect", strings.NewReader("{}"))
			r.RemoteAddr = tt.remote
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			r.Header.Set("Content-Type", tt.content)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want || called != (tt.want == 200) {
				t.Fatalf("status=%d handler called=%t", w.Code, called)
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatal("missing framing protection")
			}
		})
	}
}

func TestEmbeddedAssetsWorkOutsideSourceCheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	h, ready := platformWebHandler()
	if !ready {
		t.Fatal("embedded assets unavailable")
	}
	for _, path := range []string{"/", "/app.js", "/styles.css", "/favicon.svg"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}
