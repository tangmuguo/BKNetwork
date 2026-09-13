package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bknetwork/internal/appinfo"
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
		appinfo.DisplayName,
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
	request := httptest.NewRequest(http.MethodGet, chatGPTProxyPACURL, nil)
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

func TestValidateHomeTunnelName(t *testing.T) {
	for _, name := range []string{"home", "home-v6", "home_v6", "home.v6", "home=wg+1"} {
		if err := validateHomeTunnelName(name); err != nil {
			t.Fatalf("validateHomeTunnelName(%q) unexpected error: %v", name, err)
		}
	}
	for _, name := range []string{"", "has space", "../home", "home/route", strings.Repeat("a", 33)} {
		if err := validateHomeTunnelName(name); err == nil {
			t.Fatalf("validateHomeTunnelName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestParseHomeServiceState(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "STATE              : 1  STOPPED", want: "stopped"},
		{raw: "STATE              : 2  START_PENDING", want: "start-pending"},
		{raw: "STATE              : 4  RUNNING", want: "running"},
		{raw: "service is running", want: "running"},
		{raw: "unrecognized", want: ""},
	}
	for _, tc := range tests {
		if got := parseHomeServiceState(tc.raw); got != tc.want {
			t.Fatalf("parseHomeServiceState(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestParseHomeWireGuardDump(t *testing.T) {
	metrics, err := parseHomeWireGuardDump("private public 51820 off\npeer-one psk [2001:db8::20]:41580 0.0.0.0/0,::/0 1700000000 1234 5678 25\npeer-two psk [2001:db8::21]:41580 ::/0 1690000000 10 20 25\n")
	if err != nil {
		t.Fatalf("parseHomeWireGuardDump() error = %v", err)
	}
	if got, want := metrics.HandshakeAt, time.Unix(1700000000, 0); !got.Equal(want) {
		t.Fatalf("handshake = %v, want %v", got, want)
	}
	if metrics.ReceivedBytes != 1244 || metrics.SentBytes != 5698 {
		t.Fatalf("transfers = %d/%d, want 1244/5698", metrics.ReceivedBytes, metrics.SentBytes)
	}
}

func TestParseHomeWireGuardDumpRequiresPeer(t *testing.T) {
	if _, err := parseHomeWireGuardDump("private public 51820 off\n"); err == nil {
		t.Fatal("parseHomeWireGuardDump() unexpectedly accepted an interface-only dump")
	}
}

func TestParseHomeWireGuardMetrics(t *testing.T) {
	metrics, err := parseHomeWireGuardMetrics("peer-one 1700000000\npeer-two 1690000000\n", "peer-one 1234 5678\npeer-two 10 20\n")
	if err != nil {
		t.Fatalf("parseHomeWireGuardMetrics() error = %v", err)
	}
	if got, want := metrics.HandshakeAt, time.Unix(1700000000, 0); !got.Equal(want) {
		t.Fatalf("handshake = %v, want %v", got, want)
	}
	if metrics.ReceivedBytes != 1244 || metrics.SentBytes != 5698 {
		t.Fatalf("transfers = %d/%d, want 1244/5698", metrics.ReceivedBytes, metrics.SentBytes)
	}
}

func TestParseHomeWireGuardMetricsRequiresPeer(t *testing.T) {
	if _, err := parseHomeWireGuardMetrics("", ""); err == nil {
		t.Fatal("parseHomeWireGuardMetrics() unexpectedly accepted empty peer output")
	}
}

func TestParseHomeWireGuardAllowedIPsSpaceSeparated(t *testing.T) {
	allowed, err := parseHomeWireGuardAllowedIPs("peer-one\t0.0.0.0/0 ::/0\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(allowed, ","); got != "0.0.0.0/0,::/0" {
		t.Fatalf("allowed IPs = %q", got)
	}
	coverage := classifyHomeAllowedIPs(allowed)
	if !coverage.IPv4 || !coverage.IPv6 {
		t.Fatalf("dual-stack defaults not detected: %#v", coverage)
	}
}

func TestHomeTunnelIPv4ProbeDiagnostics(t *testing.T) {
	if targets := homeTunnelIPv4ProbeTargets(); len(targets) < 3 {
		t.Fatalf("probe target count = %d, want at least 3", len(targets))
	}
	if got := homeCounterDelta(100, 125); got != 25 {
		t.Fatalf("counter delta = %d, want 25", got)
	}
	if got := homeCounterDelta(100, 5); got != 5 {
		t.Fatalf("reset counter delta = %d, want 5", got)
	}
	for _, tc := range []struct {
		sent, received uint64
		want           string
	}{
		{sent: 0, received: 0, want: "Windows"},
		{sent: 128, received: 0, want: "不能证明数据已离开物理网卡"},
		{sent: 128, received: 64, want: "TCP"},
	} {
		if got := describeHomeProbeTraffic(tc.sent, tc.received); !strings.Contains(got, tc.want) {
			t.Fatalf("describeHomeProbeTraffic(%d, %d) = %q, want %q", tc.sent, tc.received, got, tc.want)
		}
	}
}

func TestHomeWireGuardAllowedIPCoverage(t *testing.T) {
	allowed, err := parseHomeWireGuardAllowedIPs("peer-one\t0.0.0.0/0, ::/0\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(allowed, ","); got != "0.0.0.0/0,::/0" {
		t.Fatalf("allowed IPs = %q", got)
	}
	coverage := classifyHomeAllowedIPs(allowed)
	if !coverage.IPv4 || !coverage.IPv6 {
		t.Fatalf("dual-stack defaults not detected: %#v", coverage)
	}

	splitCoverage := classifyHomeAllowedIPs([]string{"0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"})
	if !splitCoverage.IPv4 || !splitCoverage.IPv6 {
		t.Fatalf("split defaults not detected: %#v", splitCoverage)
	}
	missingIPv4 := classifyHomeAllowedIPs([]string{"::/0"})
	if missingIPv4.IPv4 || !missingIPv4.IPv6 {
		t.Fatalf("IPv6-only route classified incorrectly: %#v", missingIPv4)
	}
}

func TestContainsHomeTunnelProfile(t *testing.T) {
	profiles := []string{"Home-IPv6", "backup"}
	if !containsHomeTunnelProfile(profiles, "home-ipv6") {
		t.Fatal("profile lookup should be case insensitive")
	}
	if containsHomeTunnelProfile(profiles, "missing") {
		t.Fatal("profile lookup unexpectedly matched a missing profile")
	}
}
func TestHomeNetworkHandlerListsImportedProfiles(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("APPDATA", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	programFiles := t.TempDir()
	t.Setenv("ProgramFiles", programFiles)

	profileDir := filepath.Join(programFiles, "WireGuard", "Data", "Configurations")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The content is deliberately not a WireGuard config. The endpoint must only
	// discover the protected filename and must not read private configuration data.
	if err := os.WriteFile(filepath.Join(profileDir, "home-v6.conf.dpapi"), []byte("protected-by-wireguard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appsettings.Save(appsettings.Settings{HomeTunnelName: "home-v6"}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/home-network", nil)
	HomeNetworkHandler(nil).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HomeNetworkHandler GET status = %d; want 200", recorder.Code)
	}
	var response struct {
		OK         bool                `json:"ok"`
		TunnelName string              `json:"tunnelName"`
		Profiles   []string            `json:"profiles"`
		Status     homeNetworkSnapshot `json:"status"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.TunnelName != "home-v6" {
		t.Fatalf("unexpected response metadata: %#v", response)
	}
	if len(response.Profiles) != 1 || response.Profiles[0] != "home-v6" {
		t.Fatalf("profiles = %#v; want [home-v6]", response.Profiles)
	}
	if response.Status.TunnelName != "home-v6" {
		t.Fatalf("status tunnel = %q; want home-v6", response.Status.TunnelName)
	}
}
