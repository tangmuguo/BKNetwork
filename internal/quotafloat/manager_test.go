package quotafloat

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeProcesses struct {
	processes []Process
	addresses []string
	fail      bool
}

func (f *fakeProcesses) manager() *Manager {
	return newManager(func() ([]Process, error) {
		return append([]Process(nil), f.processes...), nil
	}, func(p Process, address string) (Process, error) {
		f.addresses = append(f.addresses, address)
		if f.fail {
			return Process{}, errors.New("cannot prepare replacement")
		}
		next := Process{PID: p.PID + 100, Started: p.Started + 1, Path: p.Path}
		for i, old := range f.processes {
			if old.id() == p.id() {
				f.processes[i] = next
				return next, nil
			}
		}
		return Process{}, errors.New("original process disappeared")
	})
}

func TestProxyEnvironmentIsProcessLocal(t *testing.T) {
	base := []string{"Path=C:\\Windows", "CODEX_HOME=C:\\custom-codex", "hTtPs_PrOxY=http://old:8080", "HTTP_PROXY=http://old:8080", "all_proxy=socks5://old:1080", "no_proxy=*"}
	original := append([]string(nil), base...)
	got := proxyEnvironment(base, "127.0.0.1:7897")
	want := []string{"Path=C:\\Windows", "CODEX_HOME=C:\\custom-codex", "HTTP_PROXY=http://127.0.0.1:7897", "HTTPS_PROXY=http://127.0.0.1:7897", "ALL_PROXY=http://127.0.0.1:7897", "NO_PROXY=localhost,127.0.0.1,::1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v; want %v", got, want)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatal("caller environment was modified")
	}
	restored := proxyEnvironment(base, "")
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("restored environment = %v", restored)
	}
	restored[0] = "changed"
	if !reflect.DeepEqual(base, original) {
		t.Fatal("restore reused caller slice")
	}
}

func TestManagerAdaptsStartupAndPortChangeOnce(t *testing.T) {
	f := &fakeProcesses{}
	m := f.manager()
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Status != "waiting" {
		t.Fatal(m.Snapshot())
	}
	f.processes = []Process{{PID: 1, Started: 10, Path: "quota-float.exe"}}
	if err := m.reconcileLocked(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Status != "active" {
		t.Fatal(m.Snapshot())
	}
	for i := 0; i < 3; i++ {
		if err := m.reconcileLocked(); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 1 {
		t.Fatalf("unchanged instance restarted %d times", len(f.addresses))
	}
	if err := m.Configure(true, "127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 2 || f.addresses[1] != "127.0.0.1:7890" {
		t.Fatal(f.addresses)
	}
	if err := m.Configure(false, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 3 || f.addresses[2] != "" {
		t.Fatal(f.addresses)
	}
	if m.Snapshot().Status != "disabled" {
		t.Fatal(m.Snapshot())
	}
	if err := m.reconcileLocked(); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 3 {
		t.Fatal("restored instance was restarted again")
	}
}

func TestManagerFailureKeepsOriginalAndWaitsForExplicitRetry(t *testing.T) {
	original := Process{PID: 7, Started: 20, Path: "quota-float.exe"}
	f := &fakeProcesses{processes: []Process{original}, fail: true}
	m := f.manager()
	if err := m.Configure(true, "127.0.0.1:7897"); err == nil {
		t.Fatal("expected restart failure")
	}
	for i := 0; i < 4; i++ {
		_ = m.reconcileLocked()
	}
	if len(f.addresses) != 1 || f.processes[0] != original {
		t.Fatal("failed instance was repeatedly restarted or removed")
	}
	if m.Snapshot().Status != "error" {
		t.Fatal(m.Snapshot())
	}
	f.fail = false
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 2 || m.Snapshot().Status != "active" {
		t.Fatal("explicit retry did not succeed")
	}
}

func TestManagerDetectsPIDReuseAndDoesNotLaunchClosedApp(t *testing.T) {
	f := &fakeProcesses{processes: []Process{{PID: 9, Started: 20}}}
	m := f.manager()
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	f.processes[0].Started += 10 // Same PID, a different process lifetime.
	if err := m.reconcileLocked(); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 2 {
		t.Fatal("PID reuse was mistaken for an adapted instance")
	}
	f.processes = nil
	if err := m.reconcileLocked(); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 2 || m.Snapshot().Status != "waiting" {
		t.Fatal("closed app should not be relaunched")
	}
}

func TestManagerDisableOnlyRestoresManagedInstances(t *testing.T) {
	f := &fakeProcesses{processes: []Process{{PID: 1, Started: 1}}}
	m := f.manager()
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	f.processes = append(f.processes, Process{PID: 2, Started: 1})
	f.fail = true
	if err := m.Configure(false, ""); err == nil {
		t.Fatal("expected restore failure")
	}
	if m.Snapshot().Status != "error" || len(f.addresses) != 2 {
		t.Fatal("restore failure was hidden or an unmanaged process was touched")
	}
	f.fail = false
	if err := m.Configure(false, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.addresses) != 3 || f.processes[1].PID != 2 {
		t.Fatal("disable changed an unmanaged process")
	}
}

func TestManagerReportsDiscoveryErrorWithoutRestarting(t *testing.T) {
	m := newManager(func() ([]Process, error) { return nil, errors.New("process discovery failed") }, func(Process, string) (Process, error) {
		t.Fatal("restart must not run after discovery fails")
		return Process{}, nil
	})
	if err := m.Configure(true, "127.0.0.1:7897"); err == nil {
		t.Fatal("expected discovery failure")
	}
	if got := m.Snapshot(); got.Status != "error" || !strings.Contains(got.Detail, "discovery failed") {
		t.Fatal(got)
	}
}

func TestRestoreRetriesOnceAndReportsManualRecovery(t *testing.T) {
	f := &fakeProcesses{processes: []Process{{PID: 3, Started: 30}}}
	m := f.manager()
	if err := m.Configure(true, "127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	f.fail = true
	if err := m.Restore(); err == nil {
		t.Fatal("expected restore error")
	}
	if len(f.addresses) != 3 {
		t.Fatalf("restore should try twice, calls=%v", f.addresses)
	}
	if snapshot := m.Snapshot(); snapshot.Status != "error" || !strings.Contains(snapshot.Detail, "完全退出并重开") {
		t.Fatal(snapshot)
	}
	f.fail = false
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Status != "disabled" {
		t.Fatal("explicit recovery did not finish")
	}
}
