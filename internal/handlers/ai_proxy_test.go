package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsettings "bknetwork/internal/settings"
)

// Execute the generated PAC, rather than reimplementing its matching in Go.
// Node is already used by the frontend regression suite; no JS dependency is
// added to the application. This test never changes Windows proxy settings.
func TestAIProxyPACRouting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required to execute the generated PAC")
	}
	proxied := []string{
		"chatgpt.com", "auth.openai.com", "ws.chatgpt.com", "cdn.oaistatic.com",
		"files.oaiusercontent.com", "challenges.cloudflare.com",
		"gemini.google.com", "GEMINI.GOOGLE.COM", "gemini.google.com.",
		"accounts.google.com", "ssl.gstatic.com", "www.gstatic.com",
		"lh3.googleusercontent.com", "lh3.googleusercontent.com.",
		"lh5.googleusercontent.com", "www.googleapis.com", "fonts.googleapis.com",
		"play.google.com", "ogs.google.com", "www.google.com", "apis.google.com",
		"jnn-pa.googleapis.com", "waa-pa.clients6.google.com", "i.ytimg.com", "yt3.ggpht.com",
		"maps.gstatic.com", "lh3.google.com", "ogads-pa.clients6.google.com",
		"csp.withgoogle.com", "www.googletagmanager.com", "www.youtube.com", "fonts.gstatic.com",
		"maps.googleapis.com", "static.doubleclick.net", "td.doubleclick.net",
		"googleads.g.doubleclick.net", "www.google-analytics.com", "optimizationguide-pa.googleapis.com",
		"encrypted-tbn0.gstatic.com", "encrypted-tbn1.gstatic.com", "encrypted-tbn2.gstatic.com",
		"encrypted-tbn3.gstatic.com", "streetviewpixels-pa.googleapis.com", "content-autofill.googleapis.com",
	}
	direct := []string{
		"", "localhost", "printer", "127.0.0.1", "::1", "192.168.1.1",
		"example.org", "google.com", "mail.google.com",
		"drive.google.com", "maps.google.com", "youtube.com",
		"googleapis.com", "storage.googleapis.com", "compute.googleapis.com",
		"gstatic.com", "unlisted.gstatic.com", "googleusercontent.com", "unlisted.googleusercontent.com",
		"child.gemini.google.com", "child.accounts.google.com", "child.lh3.googleusercontent.com",
		"gemini.google.com.evil.example", "notgemini.google.com",
		"accounts.google.com.evil.example", "evilgoogleusercontent.com",
		"chatgpt.com.evil.example", "notchatgpt.com",
	}
	type routeCase struct {
		Host string `json:"host"`
		Want string `json:"want"`
	}
	for _, address := range []string{"127.0.0.1:7897", "127.0.0.1:7890", ""} {
		t.Run("proxy="+address, func(t *testing.T) {
			var cases []routeCase
			for _, host := range proxied {
				want := "DIRECT"
				if address != "" {
					want = "PROXY " + address
				}
				cases = append(cases, routeCase{host, want})
			}
			for _, host := range direct {
				cases = append(cases, routeCase{host, "DIRECT"})
			}
			payload, err := json.Marshal(map[string]any{"pac": buildChatGPTProxyPAC(address), "cases": cases})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "-e", `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const input = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const sandbox = {
  isPlainHostName: host => !host.includes('.'),
  dnsDomainIs: (host, suffix) => host.endsWith(suffix),
};
vm.createContext(sandbox);
vm.runInContext(input.pac, sandbox, { timeout: 1000 });
for (const { host, want } of input.cases) {
  for (const scheme of ['https', 'http', 'wss']) {
    assert.equal(sandbox.FindProxyForURL(scheme + '://' + host + '/', host), want, scheme + '://' + host);
  }
}
`)
			cmd.Stdin = bytes.NewReader(payload)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PAC execution failed: %v\n%s", err, output)
			}
		})
	}
}

func TestSharedPACUsesLegacySettings(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("APPDATA", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	settingsDir := filepath.Join(configDir, "BKNetwork")
	if err := os.MkdirAll(settingsDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		settings string
		proxy    string
	}{
		{"existing enabled install", `{"chatGPTClashEnabled":true,"clashProxyAddress":"127.0.0.1:7890"}`, "127.0.0.1:7890"},
		{"existing default port", `{"chatGPTClashEnabled":true}`, defaultClashProxyAddress},
		{"disabled", `{"chatGPTClashEnabled":false,"clashProxyAddress":"127.0.0.1:7897"}`, ""},
		{"invalid address", `{"chatGPTClashEnabled":true,"clashProxyAddress":"proxy.example:8080"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(tc.settings), 0600); err != nil {
				t.Fatal(err)
			}
			// Both the pre-upgrade and refreshed URL must serve the same rules.
			for _, url := range []string{"/api/v1/chatgpt-proxy.pac?v=2.0.4", chatGPTProxyPACURL} {
				recorder := httptest.NewRecorder()
				ChatGPTProxyPACHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))
				if recorder.Code != http.StatusOK || recorder.Body.String() != buildChatGPTProxyPAC(tc.proxy) {
					t.Fatalf("legacy settings did not produce shared PAC: status %d, body %q", recorder.Code, recorder.Body.String())
				}
				if cacheControl := recorder.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
					t.Fatalf("PAC must not be cached: %q", cacheControl)
				}
			}
		})
	}
}

func TestSharedPACRecognizesPreviousURL(t *testing.T) {
	for _, url := range []string{"http://127.0.0.1:13335/api/v1/chatgpt-proxy.pac?v=2.0.4", chatGPTProxyPACURL} {
		if !isBKNetworkPAC(appsettings.SystemProxyPACState{Present: true, URL: url}) {
			t.Fatalf("upgrade must preserve restoration ownership for %s", url)
		}
	}
}
