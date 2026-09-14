package linuxnet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeResponse struct {
	out string
	err error
}

type fakeRunner struct {
	mu        sync.Mutex
	responses map[string][]fakeResponse
	calls     []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: make(map[string][]fakeResponse)}
}

func (f *fakeRunner) key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (f *fakeRunner) set(name string, args []string, responses ...fakeResponse) {
	f.responses[f.key(name, args...)] = append([]fakeResponse(nil), responses...)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(name, args...)
	f.calls = append(f.calls, key)
	responses := f.responses[key]
	if len(responses) == 0 {
		// Empty output is a safe default for read-only probes such as wg show.
		return "", nil
	}
	response := responses[0]
	if len(responses) > 1 {
		f.responses[key] = responses[1:]
	}
	return response.out, response.err
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	return "/usr/bin/" + name, nil
}

func testInterfaces(withIPv4 bool) []Interface {
	ipv4 := []string{}
	if withIPv4 {
		ipv4 = []string{"10.20.30.40/24"}
	}
	return []Interface{{
		Name:     "campus0",
		Type:     "wifi",
		State:    "connected",
		IPv4:     ipv4,
		IPv6:     []string{"2001:db8:1::20/64"},
		Physical: true,
	}}
}

func newTestManager(t *testing.T, runner *fakeRunner, interfaces []Interface) *Manager {
	t.Helper()
	return NewManagerWithOptions(t.TempDir(), Options{
		Runner:     runner,
		PathFinder: runner,
		InterfaceProvider: func(context.Context) ([]Interface, error) {
			return interfaces, nil
		},
		Privileged:   func() bool { return true },
		WireGuardDir: t.TempDir(),
	})
}

func setWarpLifecycleResponses(runner *fakeRunner) {
	runner.set("warp-cli", []string{"settings"}, fakeResponse{out: "Mode: warp+doh\n"})
	runner.set("warp-cli", []string{"--json", "status"},
		fakeResponse{out: `{"status":"Connected"}`},
		fakeResponse{out: `{"status":"Disconnected"}`},
		fakeResponse{out: `{"status":"Connected"}`},
	)
	runner.set("curl", []string{"--disable", "--noproxy", "*", "--proxy", "", "-fsS", "--connect-timeout", "5", "-6", "https://www.cloudflare.com/cdn-cgi/trace"}, fakeResponse{out: "warp=on\nip=2001:db8:1::20\n"})
	runner.set("curl", []string{"--disable", "--noproxy", "*", "--proxy", "", "-fsS", "--connect-timeout", "5", "-4", "https://www.cloudflare.com/cdn-cgi/trace"}, fakeResponse{out: "warp=on\nip=198.51.100.20\n"})
}

func TestParseWarpModeSupportsCurrentWarpCLISettings(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "legacy output", raw: "Mode: warp+doh\n", want: "warp+doh"},
		{name: "source prefixed output", raw: "(user set)\tMode: WarpWithDnsOverHttps\n", want: "warpwithdnsoverhttps"},
		{name: "unrelated setting", raw: "(default)\tAlways On: false\n", want: ""},
		{name: "embedded mode text", raw: "RemoteMode: disabled\n", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseWarpMode(test.raw); got != test.want {
				t.Fatalf("parseWarpMode() = %q, want %q", got, test.want)
			}
		})
	}
}

func setUnderlayResponses(runner *fakeRunner, restoreError error) {
	runner.set("nmcli", []string{"-t", "--escape", "no", "-f", "GENERAL.CONNECTION,GENERAL.CON-UUID,GENERAL.NM-MANAGED", "device", "show", "campus0"},
		fakeResponse{out: "GENERAL.CONNECTION:Campus\nGENERAL.CON-UUID:11111111-1111-1111-1111-111111111111\nGENERAL.NM-MANAGED:yes\n"},
		fakeResponse{out: "GENERAL.CONNECTION:Campus\nGENERAL.CON-UUID:11111111-1111-1111-1111-111111111111\nGENERAL.NM-MANAGED:yes\n"},
	)
	runner.set("nmcli", []string{"-t", "--escape", "no", "-g", "ipv4.method", "connection", "show", "uuid", "11111111-1111-1111-1111-111111111111"}, fakeResponse{out: "auto\n"})
	runner.set("ip", []string{"-4", "addr", "show", "dev", "campus0"}, fakeResponse{out: ""})
	if restoreError != nil {
		runner.set("nmcli", []string{"device", "modify", "campus0", "ipv4.method", "auto"}, fakeResponse{err: restoreError})
	} else {
		runner.set("nmcli", []string{"device", "modify", "campus0", "ipv4.method", "disabled"}, fakeResponse{} /* no-op */)
		runner.set("nmcli", []string{"device", "modify", "campus0", "ipv4.method", "auto"}, fakeResponse{})
	}
}

func TestValidateProfileNameRejectsTraversalAndShellCharacters(t *testing.T) {
	for _, profile := range []string{"../campus", "campus/blue", `campus\\blue`, "campus;rm", "campus name", ""} {
		if err := validateProfileName(profile); err == nil {
			t.Errorf("validateProfileName(%q) accepted unsafe name", profile)
		}
	}
	if err := validateProfileName("campus-v6.conf"); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

func TestRejectOtherPhysicalIPv4Egress(t *testing.T) {
	interfaces := append(testInterfaces(false), Interface{
		Name: "ethernet1", Type: "ethernet", State: "connected", IPv4: []string{"192.0.2.10/24"}, Physical: true,
	})
	if err := rejectOtherPhysicalIPv4(interfaces, "campus0"); err == nil {
		t.Fatal("expected other physical IPv4 egress to be rejected")
	}
}

func TestRejectOtherPhysicalIPv6Egress(t *testing.T) {
	interfaces := append(testInterfaces(false), Interface{
		Name: "ethernet1", Type: "ethernet", State: "connected", IPv6: []string{"2001:db8:2::10/64"}, Physical: true,
	})
	if err := rejectOtherPhysicalIPv4(interfaces, "campus0"); err == nil {
		t.Fatal("expected ambiguous second physical IPv6 egress to be rejected")
	}
}

func TestWarpLifecycleRestoresOriginalIPv4Method(t *testing.T) {
	runner := newFakeRunner()
	setWarpLifecycleResponses(runner)
	setUnderlayResponses(runner, nil)
	manager := newTestManager(t, runner, testInterfaces(true))
	if err := manager.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	state, err := manager.readState()
	if err != nil || state == nil || state.Phase != "connected" || !state.Underlay.IPv4Disabled {
		t.Fatalf("connected state = %#v, err=%v", state, err)
	}
	if err := manager.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect() error = %v", err)
	}
	if state, err := manager.readState(); err != nil || state != nil {
		t.Fatalf("state after successful disconnect = %#v, err=%v", state, err)
	}
	foundRestore := false
	for _, call := range runner.calls {
		if call == "nmcli device modify campus0 ipv4.method auto" {
			foundRestore = true
		}
	}
	if !foundRestore {
		t.Fatal("Disconnect did not restore the original IPv4 method")
	}
}

func TestConnectRollbackFailureLeavesRecoveryState(t *testing.T) {
	runner := newFakeRunner()
	runner.set("warp-cli", []string{"settings"}, fakeResponse{out: "Mode: warp+doh\n"})
	runner.set("warp-cli", []string{"connect"}, fakeResponse{err: errors.New("simulated connect failure")})
	runner.set("warp-cli", []string{"disconnect"}, fakeResponse{err: errors.New("simulated rollback failure")})
	runner.set("warp-cli", []string{"--json", "status"}, fakeResponse{out: `{"status":"Connected"}`})
	setUnderlayResponses(runner, errors.New("simulated restore failure"))
	manager := newTestManager(t, runner, testInterfaces(true))
	err := manager.Connect(context.Background(), "warp", "campus0", "")
	if err == nil || !strings.Contains(err.Error(), "自动恢复失败") {
		t.Fatalf("Connect() error = %v, want rollback failure", err)
	}
	state, readErr := manager.readState()
	if readErr != nil || state == nil || !state.RecoveryPending || state.Phase != "recovery" {
		t.Fatalf("recovery state = %#v, err=%v", state, readErr)
	}
}

func TestRestoreRefusesChangedNetworkManagerConnection(t *testing.T) {
	runner := newFakeRunner()
	runner.set("nmcli", []string{"-t", "--escape", "no", "-f", "GENERAL.CONNECTION,GENERAL.CON-UUID,GENERAL.NM-MANAGED", "device", "show", "campus0"}, fakeResponse{out: "GENERAL.CONNECTION:New\nGENERAL.CON-UUID:22222222-2222-2222-2222-222222222222\nGENERAL.NM-MANAGED:yes\n"})
	manager := newTestManager(t, runner, testInterfaces(false))
	err := manager.restoreUnderlay(context.Background(), underlaySnapshot{
		Interface: "campus0", ConnectionUUID: "11111111-1111-1111-1111-111111111111", IPv4Method: "auto", IPv4Disabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("restoreUnderlay() error = %v, want UUID mismatch", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "device modify") {
			t.Fatalf("restore attempted to modify changed connection: %s", call)
		}
	}
}

func TestVerifiedStatusRequiresRuntimeTrace(t *testing.T) {
	runner := newFakeRunner()
	runner.set("warp-cli", []string{"--json", "status"},
		fakeResponse{out: `{"status":"Connected"}`},
		fakeResponse{out: `{"status":"Connected"}`},
	)
	runner.set("curl", []string{"--disable", "--noproxy", "*", "--proxy", "", "-fsS", "--connect-timeout", "5", "-6", "https://www.cloudflare.com/cdn-cgi/trace"}, fakeResponse{out: "warp=on\nip=2001:db8:1::20\n"})
	runner.set("curl", []string{"--disable", "--noproxy", "*", "--proxy", "", "-fsS", "--connect-timeout", "5", "-4", "https://www.cloudflare.com/cdn-cgi/trace"}, fakeResponse{out: "warp=on\nip=198.51.100.20\n"})
	setUnderlayResponses(runner, nil)
	manager := newTestManager(t, runner, testInterfaces(false))
	state := persistentState{Version: 1, Mode: "warp", Interface: "campus0", Phase: "connected", UpdatedAt: timeNowForTest()}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Status(context.Background())
	if !snapshot.Verified || !snapshot.IPv6Only || snapshot.Phase != "connected" {
		t.Fatalf("status = %#v, want verified connected IPv6-only state", snapshot)
	}
}

// Kept as a small helper so tests do not depend on wall-clock literals in the
// persisted journal; the actual value is not used by the state machine.
func timeNowForTest() (now time.Time) { return time.Now().UTC() }

func TestWireGuardConfigRejectsHooksAndRequiresDualStack(t *testing.T) {
	config := `[Interface]
PrivateKey = redacted
PostUp = iptables -A OUTPUT

[Peer]
PublicKey = redacted
Endpoint = [2001:4860:4860::8888]:51820
AllowedIPs = 0.0.0.0/0, ::/0
`
	if err := validateWireGuardConfig(config); err == nil {
		t.Fatal("expected executable hook to be rejected")
	}
	if err := validateWireGuardConfig(strings.Replace(config, "PostUp = iptables -A OUTPUT\n", "", 1)); err != nil {
		t.Fatalf("safe dual-stack profile rejected: %v", err)
	}
}
