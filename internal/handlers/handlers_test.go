package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsettings "bknetwork/internal/settings"
)

func TestNormalizeClashProxyAddress(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: defaultClashProxyAddress, ok: true},
		{input: "7890", want: "127.0.0.1:7890", ok: true},
		{input: "localhost:7897", want: "127.0.0.1:7897", ok: true},
		{input: "[::1]:7897", want: "127.0.0.1:7897", ok: true},
		{input: "192.168.1.2:7897", ok: false},
		{input: "127.0.0.1:70000", ok: false},
	}
	for _, tc := range tests {
		got, err := normalizeClashProxyAddress(tc.input)
		if (err == nil) != tc.ok {
			t.Fatalf("normalizeClashProxyAddress(%q) error = %v; want ok=%v", tc.input, err, tc.ok)
		}
		if got != tc.want {
			t.Fatalf("normalizeClashProxyAddress(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestBuildChatGPTProxyPAC(t *testing.T) {
	pac := buildChatGPTProxyPAC("127.0.0.1:7897")
	for _, expected := range []string{
		`return "PROXY 127.0.0.1:7897"`,
		`"chatgpt.com"`,
		`"openai.com"`,
		`return "DIRECT"`,
	} {
		if !strings.Contains(pac, expected) {
			t.Fatalf("PAC is missing %q", expected)
		}
	}
	if disabled := buildChatGPTProxyPAC(""); strings.Contains(disabled, "PROXY ") || !strings.Contains(disabled, "DIRECT") {
		t.Fatalf("disabled PAC must always use DIRECT: %q", disabled)
	}
}

func TestChatGPTProxyPACHandler(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("APPDATA", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if err := appsettings.Save(appsettings.Settings{
		ChatGPTClashEnabled: true,
		ClashProxyAddress:   "127.0.0.1:7897",
	}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/chatgpt-proxy.pac?v=7", nil)
	ChatGPTProxyPACHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PAC handler status = %d; want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/x-ns-proxy-autoconfig") {
		t.Fatalf("PAC content type = %q", contentType)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `PROXY 127.0.0.1:7897`) || !strings.Contains(body, `"chatgpt.com"`) {
		t.Fatalf("PAC response does not contain the configured proxy and ChatGPT domains: %q", body)
	}
}

func TestParseWarpJSONStatus(t *testing.T) {
	status, reason, ok := parseWarpJSONStatus(`{"status":"Disconnected","reason":"NoNetwork"}`)
	if !ok || status != "Disconnected" || reason != "NoNetwork" {
		t.Fatalf("parseWarpJSONStatus() = %q, %q, %v", status, reason, ok)
	}
}

func TestParseWarpJSONStatusWithStructuredReason(t *testing.T) {
	status, reason, ok := parseWarpJSONStatus(`{"status":"Disconnected","reason":{"SettingsChanged":{"previous":{},"current":{}}}}`)
	if !ok || status != "Disconnected" || reason != "SettingsChanged" {
		t.Fatalf("parseWarpJSONStatus() = %q, %q, %v", status, reason, ok)
	}
}

func TestNormalizeWarpStatus(t *testing.T) {
	for _, input := range []string{"Disconnected", `"Disconnected",`, " disconnected; "} {
		if got := normalizeWarpStatus(input); got != "disconnected" {
			t.Fatalf("normalizeWarpStatus(%q) = %q", input, got)
		}
	}
}

func TestParseWarpDebugNetworkInterface(t *testing.T) {
	raw := "IPv4: [Mihomo; 198.18.0.1; Other]\nIPv6: [WLAN; 2001:db8::1; Wifi; Interface Index: 14]\n"
	if got := parseWarpDebugNetworkInterface(raw, "IPv6"); got != "WLAN" {
		t.Fatalf("parseWarpDebugNetworkInterface() = %q, want WLAN", got)
	}
}

func TestEvaluateWarpIPv6Underlay(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		ifName   string
		wantOK   bool
		wantCode string
	}{
		{
			name:     "selected IPv6 only",
			raw:      "IPv6: [WLAN; 2001:db8::1; Wifi; Interface Index: 14]\n",
			ifName:   "WLAN",
			wantOK:   true,
			wantCode: "",
		},
		{
			name:     "dual stack must not count as free flow",
			raw:      "IPv4: [WLAN; 10.23.191.136; Wifi]\nIPv6: [WLAN; 2001:db8::1; Wifi]\n",
			ifName:   "WLAN",
			wantOK:   false,
			wantCode: "warp_ipv4_underlay_still_available",
		},
		{
			name:     "wrong IPv6 interface",
			raw:      "IPv6: [Mihomo; fd00::1; Other]\n",
			ifName:   "WLAN",
			wantOK:   false,
			wantCode: "warp_ipv6_underlay_conflict",
		},
		{
			name:     "missing IPv6",
			raw:      "IPv4: [WLAN; 10.23.191.136; Wifi]\n",
			ifName:   "WLAN",
			wantOK:   false,
			wantCode: "warp_no_ipv6_underlay",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateWarpIPv6Underlay(tc.raw, tc.ifName)
			if got.OK != tc.wantOK || got.Code != tc.wantCode {
				t.Fatalf("evaluateWarpIPv6Underlay() = OK:%v Code:%q; want OK:%v Code:%q", got.OK, got.Code, tc.wantOK, tc.wantCode)
			}
		})
	}
}

func TestWarpStatusIsTerminalFailure(t *testing.T) {
	if !warpStatusIsTerminalFailure(warpSnapshot{Status: "Disconnected", Reason: "NoNetwork"}) {
		t.Fatal("NoNetwork should be treated as a terminal connection failure")
	}
	if warpStatusIsTerminalFailure(warpSnapshot{Status: "Disconnected", Reason: "Manual"}) {
		t.Fatal("Manual disconnection should not be treated as a terminal connection failure during the grace period")
	}
	if !warpStatusIsTerminalFailure(warpSnapshot{Status: "Unable", Reason: "CF_HAPPY_EYEBALLS_MITM_FAILURE"}) {
		t.Fatal("Happy Eyeballs MITM failure should end the current round so the orchestrator can retry")
	}
	if warpStatusIsTerminalFailure(warpSnapshot{Status: "Connecting", Reason: "HappyEyeballs"}) {
		t.Fatal("an in-progress Happy Eyeballs check must not end the current round")
	}
}

func TestWarpConnectionStability(t *testing.T) {
	start := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tracker := warpConnectionStability{}

	if tracker.observe(warpSnapshot{Connected: true}, start) {
		t.Fatal("a fresh Connected status must not be accepted immediately")
	}
	if tracker.observe(warpSnapshot{Connected: true}, start.Add(warpConnectedStableFor-time.Millisecond)) {
		t.Fatal("connection should not be accepted before the stability window")
	}
	if !tracker.observe(warpSnapshot{Connected: true}, start.Add(warpConnectedStableFor)) {
		t.Fatal("connection should be accepted after the full stability window")
	}

	tracker.observe(warpSnapshot{Connected: false}, start.Add(warpConnectedStableFor+time.Second))
	if tracker.observe(warpSnapshot{Connected: true}, start.Add(warpConnectedStableFor+2*time.Second)) {
		t.Fatal("a disconnect must reset the stability window")
	}
}

func TestWarpTunnelProtocolNormalization(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "MASQUE", want: "MASQUE"},
		{input: "WireGuard", want: "WireGuard"},
		{input: "WireGuard (UDP)", want: "WireGuard"},
	}
	for _, tc := range tests {
		got, err := normalizeWarpTunnelProtocol(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("normalizeWarpTunnelProtocol(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	if _, err := normalizeWarpTunnelProtocol("unknown"); err == nil {
		t.Fatal("unknown protocol should be rejected")
	}
	if !warpTunnelProtocolMatches("WireGuard (UDP)", "wireguard") {
		t.Fatal("equivalent WireGuard settings should match")
	}
}

func TestChooseRecommendedInterfaceSkipsVirtualAdapters(t *testing.T) {
	basics := []adapterBasic{
		{Name: "CloudflareWARP", Status: "Up", InterfaceDescription: "Cloudflare WARP Interface Tunnel"},
		{Name: "Mihomo", Status: "Up", InterfaceDescription: "Mihomo Virtual Adapter"},
		{Name: "WLAN", Status: "Up", InterfaceDescription: "Intel Wi-Fi 6 AX201"},
	}
	if got := chooseRecommendedInterface(basics, "CloudflareWARP", "Mihomo", "WLAN"); got != "WLAN" {
		t.Fatalf("chooseRecommendedInterface() = %q, want WLAN", got)
	}
}

func TestFreeFlowRuntimeState(t *testing.T) {
	setFreeFlowRuntimeState("warp", "WLAN")
	t.Cleanup(func() { clearFreeFlowRuntimeState("") })
	state := getFreeFlowRuntimeState()
	if state.Mode != "warp" || state.Interface != "WLAN" {
		t.Fatalf("runtime state = %#v", state)
	}
	clearFreeFlowRuntimeState("another adapter")
	if getFreeFlowRuntimeState().Interface != "WLAN" {
		t.Fatal("clearing another adapter must preserve active state")
	}
	clearFreeFlowRuntimeState("WLAN")
	if getFreeFlowRuntimeState().Interface != "" {
		t.Fatal("active adapter state should be cleared")
	}
}

func TestParseNetworkComponentBindings(t *testing.T) {
	raw := `[{"ComponentID":"ms_tcpip","Enabled":false},{"ComponentID":"ms_tcpip6","Enabled":true}]`
	ipv4, ipv6, foundIPv4, foundIPv6, err := parseNetworkComponentBindings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !foundIPv4 || !foundIPv6 {
		t.Fatalf("binding presence = IPv4:%v IPv6:%v", foundIPv4, foundIPv6)
	}
	if ipv4 || !ipv6 {
		t.Fatalf("binding state = IPv4:%v IPv6:%v; want false,true", ipv4, ipv6)
	}
}

func TestParseWarpConnected(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		want       bool
		wantStatus string
	}{
		{
			name:       "fully connected",
			raw:        "Status update: Connected\nNetwork: healthy\n",
			want:       true,
			wantStatus: "Connected",
		},
		{
			name:       "connecting",
			raw:        "Status: Connecting\n",
			want:       false,
			wantStatus: "Connecting",
		},
		{
			name:       "disabled",
			raw:        "Status: Disabled\n",
			want:       false,
			wantStatus: "Disabled",
		},
		{
			name:       "disconnected",
			raw:        "Status: Disconnected\n",
			want:       false,
			wantStatus: "Disconnected",
		},
		{
			name:       "connected but network unhealthy",
			raw:        "Status update: Connected\nNetwork: down\n",
			want:       false,
			wantStatus: "Connected",
		},
		{
			name:       "connected but network unstable",
			raw:        "Status update: Connected\nNetwork: unstable\n",
			want:       true,
			wantStatus: "Connected",
		},
		{
			name:       "empty input",
			raw:        "",
			want:       false,
			wantStatus: "",
		},
		{
			name:       "checking for update",
			raw:        "Status: Checking for update\n",
			want:       false,
			wantStatus: "Checking for update",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotStatus := parseWarpConnected(tc.raw)
			if got != tc.want {
				t.Fatalf("parseWarpConnected() connected = %v, want %v", got, tc.want)
			}
			if gotStatus != tc.wantStatus {
				t.Fatalf("parseWarpConnected() status = %q, want %q", gotStatus, tc.wantStatus)
			}
		})
	}
}

func TestSelectHighestReleaseTag(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string
		ok   bool
	}{
		{
			name: "prefer higher major version",
			tags: []string{"v0.9.9", "v1.0.0", "v0.9.8"},
			want: "v1.0.0",
			ok:   true,
		},
		{
			name: "stable beats prerelease on same core",
			tags: []string{"v1.0.0-beta.1", "v1.0.0"},
			want: "v1.0.0",
			ok:   true,
		},
		{
			name: "ignore non-semver tags",
			tags: []string{"latest", "release-2026", "v1.2.3"},
			want: "v1.2.3",
			ok:   true,
		},
		{
			name: "no valid tags",
			tags: []string{"latest", "release-2026"},
			want: "",
			ok:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectHighestReleaseTag(tc.tags)
			if ok != tc.ok {
				t.Fatalf("selectHighestReleaseTag() ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("selectHighestReleaseTag() = %q, want %q", got, tc.want)
			}
		})
	}
}
