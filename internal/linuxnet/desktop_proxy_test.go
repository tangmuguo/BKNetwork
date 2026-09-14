package linuxnet

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"bknetwork/internal/appproxy"
)

// Simulate a user's dconf bus. All NetworkManager/WARP commands still go to
// fakeRunner; no test invokes loginctl, gsettings, or a real network client.
type desktopRunner struct {
	*fakeRunner
	t           *testing.T
	mode        string
	failMode    string
	beforePause func()
}

func (r *desktopRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "runuser" {
		return r.fakeRunner.Run(ctx, name, args...)
	}
	_, _ = r.fakeRunner.Run(ctx, name, args...)
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "--user desktop-user -- /usr/bin/env ") ||
		!strings.Contains(joined, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus") ||
		!strings.Contains(joined, "XDG_RUNTIME_DIR=/run/user/1000") ||
		!strings.Contains(joined, "GSETTINGS_BACKEND=dconf") {
		r.t.Fatalf("proxy operation did not drop root into the desktop bus: %v", args)
	}
	switch {
	case strings.HasSuffix(joined, "/usr/bin/gsettings get org.gnome.system.proxy mode"):
		return "'" + r.mode + "'\n", nil
	case strings.Contains(joined, "/usr/bin/gsettings set org.gnome.system.proxy mode "):
		mode := args[len(args)-1]
		if mode == "none" && r.beforePause != nil {
			r.beforePause()
		}
		if mode == r.failMode {
			return "", errors.New("simulated dconf failure")
		}
		r.mode = mode
		return "", nil
	default:
		r.t.Fatalf("unexpected desktop command: %v", args)
		return "", nil
	}
}

func proxyManager(t *testing.T, mode string, configured bool) (*Manager, *desktopRunner, string) {
	t.Helper()
	runner := newFakeRunner()
	setWarpLifecycleResponses(runner)
	setUnderlayResponses(runner, nil)
	m := newTestManager(t, runner, testInterfaces(true))
	d := &desktopRunner{fakeRunner: runner, t: t, mode: mode}
	m.runner = d
	home := t.TempDir()
	if configured {
		path := appproxy.ConfigPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"port":7897}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.desktopUserProvider = func(context.Context) ([]desktopUser, error) {
		return []desktopUser{{UID: "1000", Name: "desktop-user", Home: home}}, nil
	}
	m.desktopAccountLookup = func(string) (*user.User, error) {
		return &user.User{Uid: "1000", Username: "desktop-user", HomeDir: home}, nil
	}
	return m, d, home
}

func TestProxyModeIgnoresGSettingsDiagnostics(t *testing.T) {
	raw := "(process:3): dconf-CRITICAL **: unable to create file\n\n'manual'\n"
	mode, err := proxyMode(raw)
	if err != nil || mode != "manual" {
		t.Fatalf("proxy mode with diagnostics = %q, %v", mode, err)
	}
}

func TestWarpPausesSystemProxyWithJournalThenRestoresIt(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "none"} {
		t.Run(mode, func(t *testing.T) {
			m, runner, _ := proxyManager(t, mode, true)
			runner.beforePause = func() {
				state, err := m.readState()
				if err != nil || state == nil || len(state.DesktopProxies) != 1 || state.DesktopProxies[0].Mode != mode || !state.DesktopProxies[0].Restore {
					t.Fatalf("proxy changed before durable snapshot: %#v %v", state, err)
				}
				for _, call := range runner.calls {
					if strings.Contains(call, "device modify") || call == "warp-cli connect" {
						t.Fatalf("network changed before proxy was paused: %v", runner.calls)
					}
				}
			}
			if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
				t.Fatal(err)
			}
			if runner.mode != "none" {
				t.Fatal("desktop-wide proxy still active in WARP")
			}
			if err := m.Disconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if runner.mode != mode {
				t.Fatalf("proxy after disconnect = %s, want %s", runner.mode, mode)
			}
			state, err := m.readState()
			if err != nil || state != nil {
				t.Fatalf("recovered journal not cleared: %#v %v", state, err)
			}
		})
	}
}

func TestUnconfiguredDesktopKeepsItsProxy(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", false)
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatal(err)
	}
	if runner.mode != "manual" {
		t.Fatal("changed a desktop that did not opt in")
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "runuser ") {
			t.Fatalf("unconfigured desktop was accessed: %s", call)
		}
	}
}

func TestWarpStartFailureRestoresProxy(t *testing.T) {
	m, runner, _ := proxyManager(t, "auto", true)
	runner.set("warp-cli", []string{"connect"}, fakeResponse{err: errors.New("simulated start failure")})
	runner.set("warp-cli", []string{"--json", "status"}, fakeResponse{out: `{"status":"Disconnected"}`})
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err == nil {
		t.Fatal("expected connection failure")
	}
	if runner.mode != "auto" {
		t.Fatal("failed connection left system proxy paused")
	}
	if state, err := m.readState(); state != nil || err != nil {
		t.Fatalf("rollback left a journal: %#v %v", state, err)
	}
}

func TestProxyPauseFailureNeverChangesUnderlayOrStartsTunnel(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	runner.failMode = "none"
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err == nil {
		t.Fatal("expected proxy pause failure")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "device modify") || call == "warp-cli connect" || call == "warp-cli disconnect" {
			t.Fatalf("desktop failure mutated networking: %s", call)
		}
	}
	if state, err := m.readState(); state != nil || err != nil {
		t.Fatalf("rollback left a journal: %#v %v", state, err)
	}
}

func TestProxyRestoreFailureKeepsRecoveryRecordAndRetries(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatal(err)
	}
	runner.set("warp-cli", []string{"--json", "status"}, fakeResponse{out: `{"status":"Disconnected"}`})
	runner.failMode = "manual"
	if err := m.Disconnect(context.Background()); err == nil {
		t.Fatal("restore failure reported as success")
	}
	state, err := m.readState()
	if err != nil || state == nil || !state.RecoveryPending || len(state.DesktopProxies) != 1 {
		t.Fatalf("missing proxy recovery record: %#v %v", state, err)
	}
	runner.failMode = ""
	if err := m.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.mode != "manual" {
		t.Fatal("retry did not restore the saved proxy")
	}
}

func TestUnconfirmedTunnelStopKeepsProxyPaused(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatal(err)
	}
	runner.set("warp-cli", []string{"--json", "status"}, fakeResponse{out: `{"status":"Disconnecting"}`})
	if err := m.Disconnect(context.Background()); err == nil || runner.mode != "none" {
		t.Fatalf("unconfirmed WARP shutdown restored proxy: mode=%s error=%v", runner.mode, err)
	}
}

func TestProxyRestoreRespectsNewUserSetting(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatal(err)
	}
	runner.mode = "auto"
	if err := m.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.mode != "auto" {
		t.Fatal("restore overwrote the user's newer setting")
	}
}

func TestProxyGuardStopsConnectBeforeMutation(t *testing.T) {
	m, runner, home := proxyManager(t, "manual", true)
	path := filepath.Join(home, ".local/share/io.github.clash-verge-rev.clash-verge-rev/verge.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("enable_proxy_guard: true # would undo suspension\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err == nil || !strings.Contains(err.Error(), "守卫") {
		t.Fatalf("proxy guard accepted: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "device modify") || call == "warp-cli connect" || strings.HasPrefix(call, "runuser ") {
			t.Fatalf("guard conflict changed the desktop/network: %s", call)
		}
	}
}

func TestOnlyActiveLocalDesktopSessionsQualify(t *testing.T) {
	valid := "User=1000\nName=desktop-user\nType=wayland\nRemote=no\nActive=yes\n"
	for _, raw := range []string{
		strings.Replace(valid, "Remote=no", "Remote=yes", 1),
		strings.Replace(valid, "Type=wayland", "Type=tty", 1),
		strings.Replace(valid, "Active=yes", "Active=no", 1),
		strings.Replace(valid, "User=1000", "User=0", 1),
		strings.Replace(valid, "User=1000", "User=01000", 1),
	} {
		if _, ok := desktopSession(raw); ok {
			t.Errorf("unsafe session accepted: %q", raw)
		}
	}
	if u, ok := desktopSession(valid); !ok || u.Name != "desktop-user" || u.UID != "1000" {
		t.Fatalf("active desktop was missed: %#v %v", u, ok)
	}
}

func TestProxyJournalValidation(t *testing.T) {
	for _, snapshot := range []desktopProxySnapshot{
		{UID: "0", User: "root", Mode: "manual"},
		{UID: "1000", User: "--help", Mode: "manual"},
		{UID: "1000", User: "desktop-user", Mode: "manual; command"},
	} {
		m, _, _ := proxyManager(t, "manual", true)
		state := persistentState{Version: 1, Mode: "warp", Phase: "recovery", DesktopProxies: []desktopProxySnapshot{snapshot}}
		if err := m.writeState(state); err != nil {
			t.Fatal(err)
		}
		if _, err := m.readState(); err == nil {
			t.Fatalf("unsafe restore record accepted: %#v", snapshot)
		}
	}
}

func TestProxyChangedDuringPreparationIsNotOverwritten(t *testing.T) {
	for _, newerMode := range []string{"none", "auto"} {
		t.Run(newerMode, func(t *testing.T) {
			m, runner, _ := proxyManager(t, "manual", true)
			snapshots, err := m.captureDesktopProxies(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			state := persistentState{Version: 1, Mode: "warp", Phase: "connecting", DesktopProxies: snapshots}
			if err := m.writeState(state); err != nil {
				t.Fatal(err)
			}
			runner.mode = newerMode
			if err := m.suspendDesktopProxies(context.Background(), &state); err == nil {
				t.Fatal("overwrote proxy changed since capture")
			}
			if err := m.restoreDesktopProxies(context.Background(), state.DesktopProxies); err != nil {
				t.Fatal(err)
			}
			if runner.mode != newerMode {
				t.Fatal("aborted preparation reverted the user's newer mode")
			}
		})
	}
}

func TestProxyRestoreRefusesReassignedUserIdentity(t *testing.T) {
	m, runner, _ := proxyManager(t, "none", true)
	m.desktopAccountLookup = func(string) (*user.User, error) {
		return &user.User{Uid: "1000", Username: "somebody-else"}, nil
	}
	err := m.restoreDesktopProxies(context.Background(), []desktopProxySnapshot{{UID: "1000", User: "desktop-user", Mode: "manual", Restore: true}})
	if err == nil || len(runner.calls) != 0 {
		t.Fatalf("accessed a reassigned user's bus: error=%v calls=%v", err, runner.calls)
	}
}

func TestActiveClashTUNIsRejectedBeforeAnyProxyChange(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	m.interfaceProvider = func(context.Context) ([]Interface, error) {
		return append(testInterfaces(true), Interface{Name: "Meta", Type: "tun", State: "connected"}), nil
	}
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err == nil || !strings.Contains(err.Error(), "Meta") {
		t.Fatalf("active TUN conflict not detected: %v", err)
	}
	if runner.mode != "manual" {
		t.Fatal("TUN conflict changed the desktop proxy")
	}
	if state, err := m.readState(); err != nil || state != nil {
		t.Fatalf("TUN conflict wrote state: %#v %v", state, err)
	}
}

func TestAlreadyDisconnectedWarpStillRestoresProxy(t *testing.T) {
	m, runner, _ := proxyManager(t, "manual", true)
	if err := m.Connect(context.Background(), "warp", "campus0", ""); err != nil {
		t.Fatal(err)
	}
	runner.set("warp-cli", []string{"disconnect"}, fakeResponse{err: errors.New("already disconnected")})
	runner.set("warp-cli", []string{"--json", "status"}, fakeResponse{out: `{"status":"Disconnected"}`})
	if err := m.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.mode != "manual" {
		t.Fatal("confirmed disconnection did not restore system proxy")
	}
}

func TestProxyGuardParsing(t *testing.T) {
	for _, tc := range []struct {
		name, yaml       string
		enabled, invalid bool
	}{
		{"disabled", "enable_proxy_guard: false\n", false, false},
		{"bom", "\ufeffenable_proxy_guard: true\n", true, false},
		{"whitespace", "  enable_proxy_guard: true\n", true, false},
		{"quoted", "'enable_proxy_guard': true # comment\n", true, false},
		{"duplicate", "enable_proxy_guard: false\nenable_proxy_guard: true\n", false, true},
		{"unknown", "enable_proxy_guard: [false]\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".local/share/io.github.clash-verge-rev.clash-verge-rev/verge.yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			enabled, err := clashProxyGuardEnabled(home)
			if enabled != tc.enabled || (err != nil) != tc.invalid {
				t.Fatalf("guard=%v error=%v", enabled, err)
			}
		})
	}
}
