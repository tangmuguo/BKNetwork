package linuxnet

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func testAppProxyDesktopUser(t *testing.T, manager *Manager, home string) {
	t.Helper()
	manager.desktopUserProvider = func(context.Context) ([]desktopUser, error) {
		return []desktopUser{{UID: "1000", Name: "desktop-user", Home: home}}, nil
	}
	manager.desktopAccountLookup = func(string) (*user.User, error) {
		return &user.User{Uid: "1000", Username: "desktop-user", HomeDir: home}, nil
	}
}

func TestSetClashAppProxyRunsAsDesktopUser(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(true))
	home := t.TempDir()
	testAppProxyDesktopUser(t, manager, home)
	manager.appProxyBinary = "/opt/bknetwork/bknetwork"

	if err := manager.SetClashAppProxy(context.Background(), true, 7897); err != nil {
		t.Fatalf("SetClashAppProxy(enable) error = %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "runuser --user desktop-user -- /usr/bin/env") || !strings.Contains(joined, "HOME="+home) || !strings.Contains(joined, "/opt/bknetwork/bknetwork app-proxy install --port 7897") {
		t.Fatalf("enable command did not run as desktop user: %s", joined)
	}
}

func TestSetClashAppProxyDisablesExistingConfiguration(t *testing.T) {
	runner := newFakeRunner()
	manager := newTestManager(t, runner, testInterfaces(true))
	home := t.TempDir()
	testAppProxyDesktopUser(t, manager, home)
	manager.appProxyBinary = "/opt/bknetwork/bknetwork"
	configPath := filepath.Join(home, ".local/share/bknetwork/app-proxy/config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":1,"port":7897}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := manager.SetClashAppProxy(context.Background(), false, 7897); err != nil {
		t.Fatalf("SetClashAppProxy(disable) error = %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "/opt/bknetwork/bknetwork app-proxy remove") {
		t.Fatalf("disable command did not remove the wrapper: %s", joined)
	}
}
