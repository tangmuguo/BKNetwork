package linuxnet

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUnmanagedDisconnectNeverRunsCommands(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(true))
	if err := manager.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unmanaged network was touched: %v", runner.calls)
	}
}

func TestProfilesRejectOptionNamesAndSymlinkDirectory(t *testing.T) {
	for _, name := range []string{"--help", "-", ".", "..", "/tmp/home"} {
		if validateProfileName(name) == nil {
			t.Errorf("unsafe profile name %q accepted", name)
		}
	}
	manager := newTestManager(t, newFakeRunner(), testInterfaces(false))
	link := filepath.Join(t.TempDir(), "wireguard")
	if err := os.Symlink(manager.wireGuardDir, link); err != nil {
		t.Fatal(err)
	}
	manager.wireGuardDir = link
	if _, err := manager.listProfiles(); err == nil {
		t.Fatal("symlinked profile directory accepted")
	}
	if _, err := manager.validateAndResolveProfile("home"); err == nil {
		t.Fatal("symlinked profile directory allowed for execution")
	}
}

func TestSaveConfigCommentCannotBypassValidation(t *testing.T) {
	config := "[Interface]\nSaveConfig = true # overwrites file\n[Peer]\nEndpoint = [2606:4700::1111]:51820\nAllowedIPs = 0.0.0.0/0, ::/0 # full tunnel\n"
	if validateWireGuardConfig(config) == nil {
		t.Fatal("SaveConfig inline comment bypassed validation")
	}
	if err := validateWireGuardConfig(strings.Replace(config, "true # overwrites file", "false", 1)); err != nil {
		t.Fatalf("valid commented config rejected: %v", err)
	}
}

func TestEveryWireGuardEndpointMustUseSelectedUnderlay(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(false))
	runner.set("wg", []string{"show", "home", "endpoints"}, fakeResponse{out: "peer1 [2606:4700::1111]:51820\npeer2 [2606:4700::1001]:51820\n"})
	runner.set("wg", []string{"show", "home", "fwmark"}, fakeResponse{out: "0xca6c"})
	runner.set("ip", []string{"-6", "route", "get", "2606:4700::1111", "mark", "0xca6c"}, fakeResponse{out: "2606:4700::1111 dev campus0"})
	runner.set("ip", []string{"-6", "route", "get", "2606:4700::1001", "mark", "0xca6c"}, fakeResponse{out: "2606:4700::1001 dev home"})
	if manager.wireGuardEndpointOnIPv6(context.Background(), "home", "campus0") {
		t.Fatal("second peer loops into tunnel but was accepted")
	}
}

func TestWireGuardVerificationNeedsDualStackAndFreshHandshake(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(false))
	runner.set("wg", []string{"show", "interfaces"}, fakeResponse{out: "home"})
	runner.set("wg", []string{"show", "home", "latest-handshakes"}, fakeResponse{out: "public-peer " + strconv.FormatInt(time.Now().Unix(), 10)})
	runner.set("wg", []string{"show", "home", "endpoints"}, fakeResponse{out: "public-peer [2606:4700::1111]:51820"})
	runner.set("wg", []string{"show", "home", "fwmark"}, fakeResponse{out: "0xca6c"})
	runner.set("ip", []string{"-6", "route", "get", "2606:4700::1111", "mark", "0xca6c"}, fakeResponse{out: "2606:4700::1111 dev campus0"})
	runner.set("ip", []string{"-4", "route", "get", "1.1.1.1"}, fakeResponse{out: "1.1.1.1 dev home"})
	runner.set("ip", []string{"-6", "route", "get", "2606:4700:4700::1111"}, fakeResponse{out: "2606:4700:4700::1111 dev home"})
	if ok, detail := manager.verifyRuntime(context.Background(), "wireguard", "campus0", "home"); !ok {
		t.Fatalf("valid simulated tunnel rejected: %s", detail)
	}
	runner.set("ip", []string{"-4", "route", "get", "1.1.1.1"}, fakeResponse{out: "1.1.1.1 dev campus0"})
	if ok, _ := manager.verifyRuntime(context.Background(), "wireguard", "campus0", "home"); ok {
		t.Fatal("IPv4 route bypass accepted")
	}
	runner.set("wg", []string{"show", "home", "latest-handshakes"}, fakeResponse{out: "public-peer 0"})
	if ok, _ := manager.verifyRuntime(context.Background(), "wireguard", "campus0", "home"); ok {
		t.Fatal("unestablished handshake accepted")
	}
}

func TestPendingRecoveryCannotBeReplacedByConnect(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(true))
	state := persistentState{Version: 1, Mode: "warp", Interface: "campus0", Phase: "error", RecoveryPending: true}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(manager.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Connect(context.Background(), "warp", "campus0", ""); err == nil {
		t.Fatal("pending recovery overwritten")
	}
	after, err := os.ReadFile(manager.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(runner.calls) != 0 {
		t.Fatal("new operation modified pending recovery")
	}
}

func TestOnlyDefiniteWarpDisconnectionPermitsRecovery(t *testing.T) {
	for _, status := range []string{"Disconnecting", "RegistrationPending", "Unable to disconnect", "Connected", "", "Error"} {
		if warpStatusDefinitelyDisconnected(status) {
			t.Errorf("%q treated as disconnected", status)
		}
	}
	if !warpStatusDefinitelyDisconnected("Disconnected") {
		t.Fatal("Disconnected not recognized")
	}
}

func TestEndpointRejectsZeroPortAndNonPublicTransport(t *testing.T) {
	for _, endpoint := range []string{"[2606:4700::1111]:00", "[2606:4700::1111]:0", "[2606:4700::1111]:65536", "192.0.2.1:51820", "server.example:51820", "[::1]:51820", "[fe80::1]:51820", "[fd00::1]:51820", "[ff02::1]:51820"} {
		if err := validatePublicIPv6Endpoint(endpoint); err == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
}

func TestCorruptJournalBlocksAllMutations(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(true))
	if err := os.Chmod(manager.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Disconnect(context.Background()); err == nil {
		t.Fatal("ignored corrupt journal")
	}
	if err := manager.Connect(context.Background(), "warp", "campus0", ""); err == nil {
		t.Fatal("ignored corrupt journal on connect")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("mutated network despite corrupt journal: %v", runner.calls)
	}
}
