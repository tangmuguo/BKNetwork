package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const homeRoutingScriptPrefix = "# BKNetwork home-routing: "

type homeRoutingFakeCall struct {
	action string
	script string
}

// homeRoutingFake is deliberately a command boundary fake. It recognizes the
// marker required by homeRoutingManager and never invokes PowerShell, netsh,
// or any other system command.
type homeRoutingFake struct {
	mu sync.Mutex

	snapshot homeRoutingState

	routeExistsDefault bool
	routeExists        map[string]bool

	failAction string
	failAfter  int
	failErr    error

	calls []homeRoutingFakeCall
}

func (f *homeRoutingFake) run(_ context.Context, _ string, args ...string) (string, error) {
	script := strings.Join(args, "\n")
	action := homeRoutingFakeAction(script)
	if action == "" {
		return "", fmt.Errorf("home-routing fake did not find action marker in %q", script)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, homeRoutingFakeCall{action: action, script: script})

	if f.failAction == action {
		seen := 0
		for _, call := range f.calls {
			if call.action == action {
				seen++
			}
		}
		if f.failAfter <= 0 || seen >= f.failAfter {
			if f.failErr != nil {
				return "", f.failErr
			}
			return "", errors.New("fake command failure")
		}
	}

	switch action {
	case "snapshot":
		state := f.snapshot
		state.TunnelName = ""
		encoded, err := json.Marshal(state)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case "route-exists":
		for destination, exists := range f.routeExists {
			if strings.Contains(script, destination) {
				encoded, err := json.Marshal(exists)
				return string(encoded), err
			}
		}
		encoded, err := json.Marshal(f.routeExistsDefault)
		return string(encoded), err
	default:
		return "", nil
	}
}

func homeRoutingFakeAction(script string) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, homeRoutingScriptPrefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, homeRoutingScriptPrefix))
	}
	return ""
}

func (f *homeRoutingFake) actionCalls(action string) []homeRoutingFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]homeRoutingFakeCall, 0)
	for _, call := range f.calls {
		if call.action == action {
			result = append(result, call)
		}
	}
	return result
}

func (f *homeRoutingFake) allCalls() []homeRoutingFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]homeRoutingFakeCall(nil), f.calls...)
}

func newHomeRoutingTestManager(t *testing.T, fake *homeRoutingFake) (*homeRoutingManager, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "home-routing-state.json")
	return &homeRoutingManager{statePath: statePath, run: fake.run}, statePath
}

func sampleHomeRoutingState() homeRoutingState {
	return homeRoutingState{
		InterfaceName:  "WLAN",
		InterfaceGUID:  "00000000-0000-0000-0000-000000000001",
		InterfaceIndex: 14,
		Forwarding:     true,
		WeakHostSend:   true,
		NextHop:        "fe80::1",
	}
}

func sampleHomeEndpoint(t *testing.T, raw string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", raw, err)
	}
	return addr
}

func readHomeRoutingState(t *testing.T, path string) homeRoutingState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var state homeRoutingState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("Unmarshal state: %v", err)
	}
	return state
}

func assertHomeRoutingStateExists(t *testing.T, path string) homeRoutingState {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}
	return readHomeRoutingState(t, path)
}

func assertHomeRoutingStateRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file should be removed, stat error = %v", err)
	}
}

func assertHomeRoutingScriptsAreIPv6Only(t *testing.T, fake *homeRoutingFake) {
	t.Helper()
	for _, call := range fake.allCalls() {
		if call.action != "disable" && call.action != "restore" {
			continue
		}
		if !strings.Contains(call.script, "-AddressFamily IPv6") {
			t.Fatalf("%s script did not select IPv6: %s", call.action, call.script)
		}
		if strings.Contains(call.script, "-AddressFamily IPv4") ||
			strings.Contains(call.script, "ms_tcpip") ||
			strings.Contains(call.script, "ms_tcpip6") {
			t.Fatalf("%s script touched adapter bindings or IPv4: %s", call.action, call.script)
		}
	}
}

func TestHomeRoutingLifecycleProtectsOnlyNewEndpointRoutes(t *testing.T) {
	fake := &homeRoutingFake{snapshot: sampleHomeRoutingState()}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()
	endpoint := sampleHomeEndpoint(t, "2402:4e00:c050:1e00:41c:1f70:c220:0")

	if err := manager.prepare(ctx, "WLAN", "BKNetwork-ganlu"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	saved := assertHomeRoutingStateExists(t, statePath)
	if saved.TunnelName != "BKNetwork-ganlu" || saved.InterfaceName != "WLAN" {
		t.Fatalf("saved identity = %#v", saved)
	}
	if !saved.Forwarding || !saved.WeakHostSend || saved.InterfaceIndex != 14 {
		t.Fatalf("original interface state was not preserved: %#v", saved)
	}

	if err := manager.protectEndpoints(ctx, "BKNetwork-ganlu", []netip.Addr{endpoint}); err != nil {
		t.Fatalf("protectEndpoints: %v", err)
	}
	saved = readHomeRoutingState(t, statePath)
	if len(saved.Routes) != 1 {
		t.Fatalf("new route count = %d, want 1 (%#v)", len(saved.Routes), saved.Routes)
	}
	route := saved.Routes[0]
	if route.DestinationPrefix != endpoint.String()+"/128" ||
		route.NextHop != "fe80::1" || route.InterfaceIndex != 14 || (route.RouteMetric < 32768 || route.RouteMetric > 65535) {
		t.Fatalf("saved endpoint route = %#v", route)
	}
	for _, call := range fake.actionCalls("route-add") {
		if !strings.Contains(call.script, "DestinationPrefix '"+endpoint.String()+"/128'") ||
			!strings.Contains(call.script, "-NextHop 'fe80::1'") ||
			!strings.Contains(call.script, "-InterfaceIndex 14") {
			t.Fatalf("route-add did not use the physical IPv6 next hop/interface: %s", call.script)
		}
	}
	assertHomeRoutingScriptsAreIPv6Only(t, fake)

	if err := manager.restore(ctx, "BKNetwork-ganlu"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertHomeRoutingStateRemoved(t, statePath)
	for _, call := range fake.actionCalls("route-remove") {
		if !strings.Contains(call.script, "00000000-0000-0000-0000-000000000001") ||
			!strings.Contains(call.script, "-InterfaceIndex 14") {
			t.Fatalf("route-remove did not retain saved adapter identity: %s", call.script)
		}
	}
	if len(fake.actionCalls("route-remove")) != 1 {
		t.Fatalf("route-remove calls = %d, want 1", len(fake.actionCalls("route-remove")))
	}
	if len(fake.actionCalls("restore")) != 1 {
		t.Fatalf("restore calls = %d, want 1", len(fake.actionCalls("restore")))
	}
}

func TestHomeRoutingPreparePersistsSnapshotBeforeDisableFailure(t *testing.T) {
	fake := &homeRoutingFake{
		snapshot:   sampleHomeRoutingState(),
		failAction: "disable",
		failErr:    errors.New("simulated disable crash"),
	}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()

	if err := manager.prepare(ctx, "WLAN", "home"); err == nil {
		t.Fatal("prepare unexpectedly succeeded after disable failure")
	}
	saved := assertHomeRoutingStateExists(t, statePath)
	if saved.TunnelName != "home" || saved.InterfaceName != "WLAN" {
		t.Fatalf("snapshot was not persisted before disable: %#v", saved)
	}
	if len(fake.actionCalls("disable")) != 1 {
		t.Fatalf("disable calls = %d, want 1", len(fake.actionCalls("disable")))
	}

	// A fresh manager must be able to recover the persisted snapshot after a
	// process restart; it must not take a new snapshot or overwrite the flags.
	restarted := &homeRoutingManager{statePath: statePath, run: fake.run}
	fake.failAction = ""
	if err := restarted.restore(ctx, "home"); err != nil {
		t.Fatalf("restore after simulated restart: %v", err)
	}
	assertHomeRoutingStateRemoved(t, statePath)
	if len(fake.actionCalls("snapshot")) != 1 {
		t.Fatalf("snapshot calls = %d, want exactly 1", len(fake.actionCalls("snapshot")))
	}
	assertHomeRoutingScriptsAreIPv6Only(t, fake)
}

func TestHomeRoutingPrepareRepeatPreservesOriginalSnapshot(t *testing.T) {
	fake := &homeRoutingFake{snapshot: sampleHomeRoutingState()}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()

	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	first := readHomeRoutingState(t, statePath)

	// A second invocation with the same identity must be idempotent. Change the
	// fake snapshot to prove that prepare does not replace the original state.
	fake.snapshot.Forwarding = false
	fake.snapshot.WeakHostSend = false
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("repeated prepare: %v", err)
	}
	second := readHomeRoutingState(t, statePath)
	if second.Forwarding != first.Forwarding || second.WeakHostSend != first.WeakHostSend {
		t.Fatalf("repeated prepare overwrote original flags: first=%#v second=%#v", first, second)
	}
	if len(fake.actionCalls("snapshot")) != 1 || len(fake.actionCalls("disable")) != 2 {
		t.Fatalf("repeated prepare should preserve the snapshot and re-assert IPv6 flags: calls=%#v", fake.allCalls())
	}
	assertHomeRoutingScriptsAreIPv6Only(t, fake)

	if err := manager.prepare(ctx, "WLAN", "other"); err == nil {
		t.Fatal("prepare unexpectedly accepted a different tunnel")
	}
	if err := manager.prepare(ctx, "Ethernet", "home"); err == nil {
		t.Fatal("prepare unexpectedly accepted a different interface")
	}
	if got := readHomeRoutingState(t, statePath); got.TunnelName != "home" || got.InterfaceName != "WLAN" {
		t.Fatalf("conflicting prepare changed persisted identity: %#v", got)
	}
}

func TestHomeRoutingProtectPartialRouteAddCanBeRestored(t *testing.T) {
	fake := &homeRoutingFake{
		snapshot:   sampleHomeRoutingState(),
		failAction: "route-add",
		failAfter:  2,
		failErr:    errors.New("simulated second route add failure"),
	}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	endpoints := []netip.Addr{
		sampleHomeEndpoint(t, "2001:db8::1"),
		sampleHomeEndpoint(t, "2001:db8::2"),
	}
	if err := manager.protectEndpoints(ctx, "home", endpoints); err == nil {
		t.Fatal("protectEndpoints unexpectedly succeeded after partial route add")
	}

	saved := assertHomeRoutingStateExists(t, statePath)
	if len(saved.Routes) != 2 {
		t.Fatalf("partial route state = %#v, want both attempted routes recorded for idempotent cleanup", saved.Routes)
	}
	fake.failAction = ""
	if err := manager.restore(ctx, "home"); err != nil {
		t.Fatalf("restore partial route state: %v", err)
	}
	assertHomeRoutingStateRemoved(t, statePath)
	if len(fake.actionCalls("route-remove")) != 2 {
		t.Fatalf("route-remove calls = %d, want 2", len(fake.actionCalls("route-remove")))
	}
	assertHomeRoutingScriptsAreIPv6Only(t, fake)
}

func TestHomeRoutingPreservesExistingEndpointRoute(t *testing.T) {
	fake := &homeRoutingFake{
		snapshot:           sampleHomeRoutingState(),
		routeExistsDefault: true,
	}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	endpoints := []netip.Addr{sampleHomeEndpoint(t, "2001:db8::1"), sampleHomeEndpoint(t, "2001:db8::2")}
	if err := manager.protectEndpoints(ctx, "home", endpoints); err != nil {
		t.Fatalf("protectEndpoints: %v", err)
	}
	saved := readHomeRoutingState(t, statePath)
	if len(saved.Routes) != 0 {
		t.Fatalf("pre-existing routes were recorded for removal: %#v", saved.Routes)
	}
	if len(fake.actionCalls("route-add")) != 0 {
		t.Fatalf("route-add calls = %d, want 0", len(fake.actionCalls("route-add")))
	}
	if err := manager.restore(ctx, "home"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(fake.actionCalls("route-remove")) != 0 {
		t.Fatalf("route-remove calls = %d, want 0", len(fake.actionCalls("route-remove")))
	}
	assertHomeRoutingStateRemoved(t, statePath)
	assertHomeRoutingScriptsAreIPv6Only(t, fake)
}

func TestHomeRoutingRestoreFailureRetainsState(t *testing.T) {
	fake := &homeRoutingFake{
		snapshot:   sampleHomeRoutingState(),
		failAction: "route-remove",
		failErr:    errors.New("simulated route removal failure"),
	}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := manager.protectEndpoints(ctx, "home", []netip.Addr{sampleHomeEndpoint(t, "2001:db8::1")}); err != nil {
		t.Fatalf("protectEndpoints: %v", err)
	}
	want := readHomeRoutingState(t, statePath)
	if err := manager.restore(ctx, "home"); err == nil {
		t.Fatal("restore unexpectedly succeeded after route removal failure")
	}
	got := assertHomeRoutingStateExists(t, statePath)
	if got.TunnelName != want.TunnelName || len(got.Routes) != len(want.Routes) {
		t.Fatalf("failed restore lost recovery state: before=%#v after=%#v", want, got)
	}

	fake.failAction = ""
	if err := manager.restore(ctx, "home"); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	assertHomeRoutingStateRemoved(t, statePath)
	assertHomeRoutingScriptsAreIPv6Only(t, fake)
}

func TestHomeRoutingRejectsInvalidEndpointCollectionsWithoutCommands(t *testing.T) {
	fake := &homeRoutingFake{snapshot: sampleHomeRoutingState()}
	manager, statePath := newHomeRoutingTestManager(t, fake)
	ctx := context.Background()
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cases := []struct {
		name      string
		endpoints []netip.Addr
	}{
		{name: "nil", endpoints: nil},
		{name: "empty", endpoints: []netip.Addr{}},
		{name: "ipv4", endpoints: []netip.Addr{netip.MustParseAddr("192.0.2.1")}},
		{name: "zero-value", endpoints: []netip.Addr{netip.Addr{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fake.allCalls())
			if err := manager.protectEndpoints(ctx, "home", tc.endpoints); err == nil {
				t.Fatalf("protectEndpoints(%s) unexpectedly succeeded", tc.name)
			}
			if got := len(fake.allCalls()); got != before {
				t.Fatalf("invalid endpoint input issued commands: before=%d after=%d calls=%#v", before, got, fake.allCalls())
			}
		})
	}
	if state := readHomeRoutingState(t, statePath); len(state.Routes) != 0 {
		t.Fatalf("invalid endpoint input changed state: %#v", state.Routes)
	}
}

func TestHomeRoutingRejectsInvalidPersistedIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*homeRoutingState)
	}{
		{
			name: "unmatched opening brace",
			mutate: func(state *homeRoutingState) {
				state.InterfaceGUID = "{" + state.InterfaceGUID
			},
		},
		{
			name: "unmatched closing brace",
			mutate: func(state *homeRoutingState) {
				state.InterfaceGUID += "}"
			},
		},
		{
			name: "malformed UUID",
			mutate: func(state *homeRoutingState) {
				state.InterfaceGUID = "00000000-0000-0000-0000-00000000000x"
			},
		},
		{
			name: "invalid next hop",
			mutate: func(state *homeRoutingState) {
				state.NextHop = "192.0.2.1"
			},
		},
		{
			name: "zero interface index",
			mutate: func(state *homeRoutingState) {
				state.InterfaceIndex = 0
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &homeRoutingFake{snapshot: sampleHomeRoutingState()}
			manager, statePath := newHomeRoutingTestManager(t, fake)
			state := sampleHomeRoutingState()
			state.TunnelName = "home"
			tc.mutate(&state)
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatalf("Marshal state: %v", err)
			}
			if err := os.WriteFile(statePath, raw, 0o600); err != nil {
				t.Fatalf("WriteFile state: %v", err)
			}

			if err := manager.prepare(context.Background(), "WLAN", "home"); err == nil {
				t.Fatal("prepare unexpectedly accepted invalid persisted identity")
			}
			if calls := fake.allCalls(); len(calls) != 0 {
				t.Fatalf("invalid persisted state issued commands: %#v", calls)
			}
		})
	}
}

func TestHomeTunnelRoutesScriptRequiresCampusIPv6OnlyAndDualStackTunnelRoutes(t *testing.T) {
	script := homeTunnelRoutesScript("WLAN", "BKNetwork-ganlu")
	required := []string{
		"Get-NetAdapterBinding",
		"ComponentID ms_tcpip,ms_tcpip6",
		"$v4[0].Enabled",
		"$v6[0].Enabled",
		"0.0.0.0/0",
		"0.0.0.0/1",
		"128.0.0.0/1",
		"::/0",
		"::/1",
		"8000::/1",
		"Find-NetRoute",
		"1.1.1.1",
		"2606:4700:4700::1111",
		"Get-NetRoute -InterfaceIndex $t[0].ifIndex",
	}
	for _, token := range required {
		if !strings.Contains(script, token) {
			t.Fatalf("homeTunnelRoutesScript missing %q: %s", token, script)
		}
	}
	if !strings.Contains(script, "-not $v6[0].Enabled") {
		t.Fatalf("script does not require the physical IPv6 binding: %s", script)
	}
	if !strings.Contains(script, "$v4[0].Enabled") {
		t.Fatalf("script does not reject an enabled physical IPv4 binding: %s", script)
	}

	escaped := homeTunnelRoutesScript("WLAN'evil", "BKNetwork'evil")
	if !strings.Contains(escaped, "WLAN''evil") || !strings.Contains(escaped, "BKNetwork''evil") {
		t.Fatalf("adapter/tunnel names were not PowerShell-escaped: %s", escaped)
	}
	if strings.Contains(escaped, "Name -eq 'WLAN'evil'") ||
		strings.Contains(escaped, "Name -eq 'BKNetwork'evil'") {
		t.Fatalf("unescaped single-quoted input reached the route script: %s", escaped)
	}
}

func TestParseHomeWireGuardEndpointsDeduplicatesIPv6(t *testing.T) {
	raw := "peer-one\t[2402:4e00:c050:1e00:41c:1f70:c220:0]:51820\n" +
		"peer-two\t[2001:db8::1]:51820\n" +
		"peer-three\t[2001:db8::1]:51820\n"
	got, err := parseHomeWireGuardEndpoints(raw)
	if err != nil {
		t.Fatalf("parseHomeWireGuardEndpoints: %v", err)
	}
	want := []netip.Addr{
		netip.MustParseAddr("2402:4e00:c050:1e00:41c:1f70:c220:0"),
		netip.MustParseAddr("2001:db8::1"),
	}
	if len(got) != len(want) {
		t.Fatalf("endpoint count = %d, want %d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestParseHomeWireGuardEndpointsRejectsInvalidOrUnsafeData(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "ipv4", raw: "peer\t192.0.2.1:51820\n"},
		{name: "none", raw: "peer\t(none)\n"},
		{name: "hostname", raw: "peer\tvpn.example.com:51820\n"},
		{name: "missing-port", raw: "peer\t[2001:db8::1]\n"},
		{name: "zero-port", raw: "peer\t[2001:db8::1]:0\n"},
		{name: "port-overflow", raw: "peer\t[2001:db8::1]:65536\n"},
		{name: "command-injection", raw: "peer\t[2001:db8::1]:51820; Remove-NetRoute\n"},
		{name: "private-key-line", raw: "PrivateKey = super-secret\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHomeWireGuardEndpoints(tc.raw)
			if err == nil {
				t.Fatalf("parseHomeWireGuardEndpoints unexpectedly accepted %q as %#v", tc.raw, got)
			}
			if strings.Contains(strings.Join(func() []string {
				values := make([]string, 0, len(got))
				for _, endpoint := range got {
					values = append(values, endpoint.String())
				}
				return values
			}(), " "), "super-secret") {
				t.Fatal("private key material appeared in parsed endpoint output")
			}
		})
	}
}

func TestHomeTunnelStopConfirmationDistinguishesCleanupFromLiveTunnel(t *testing.T) {
	if !homeTunnelStopConfirmed(nil) {
		t.Fatal("successful stop must permit normal network restore")
	}
	if !homeTunnelStopConfirmed(fmt.Errorf("wrapped: %w", &homeRoutingRestoreError{err: errors.New("cleanup failed")})) {
		t.Fatal("stopped tunnel with pending cleanup must permit normal network restore")
	}
	if homeTunnelStopConfirmed(errors.New("service stop timed out")) {
		t.Fatal("a possibly live tunnel must retain protected network state")
	}
}
