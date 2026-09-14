package server

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Localhost alone is not a CSRF boundary. Validate the Host to reject DNS
// rebinding and the Origin and content type before any privileged mutation.
func localOnly(next http.Handler, address string) http.Handler {
	_, port, _ := net.SplitHostPort(address)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		host, requestPort, err := net.SplitHostPort(r.Host)
		if err != nil || requestPort != port || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
			http.Error(w, "invalid local host", http.StatusForbidden)
			return
		}
		if remote, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
				http.Error(w, "local clients only", http.StatusForbidden)
				return
			}
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || u.Host != r.Host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				http.Error(w, "same-origin requests only", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			w.Header().Set("Cache-Control", "no-store")
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if mediaType != "application/json" {
					http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			}
		}
		next.ServeHTTP(w, r)
	})
}
