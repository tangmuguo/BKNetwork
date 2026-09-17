package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCombinedAdapters = `{"Adapters":[{"Name":"WLAN","Status":"Up","MacAddress":"00-11-22-33-44-55","InterfaceDescription":"Wireless"}],"Bindings":[{"Name":"WLAN","ComponentID":"ms_tcpip","Enabled":false},{"Name":"WLAN","ComponentID":"ms_tcpip6","Enabled":true}]}`

func TestSnapshotAdaptersUseOneProcessAndPreserveBindings(t *testing.T) {
	calls := 0
	raw, ipv4, ipv6 := collectSnapshotAdapterData(func(ctx context.Context, name string, args ...string) (string, error) {
		calls++
		if name != "powershell" || args[len(args)-1] != snapshotAdaptersCommand {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("query has no timeout")
		}
		return testCombinedAdapters, nil
	})
	basics, err := decodeJSONList[adapterBasic](raw)
	if err != nil || len(basics) != 1 || basics[0].Name != "WLAN" {
		t.Fatalf("adapter metadata: %s, %v", raw, err)
	}
	if calls != 1 || ipv4["WLAN"] || !ipv6["WLAN"] {
		t.Fatalf("calls=%d, ipv4=%v, ipv6=%v", calls, ipv4, ipv6)
	}
	if _, exists := ipv4["WLAN"]; !exists {
		t.Fatal("disabled binding omitted")
	}
}

func TestSnapshotAdaptersFallbackPreservesIndependentResults(t *testing.T) {
	for _, combined := range []string{"", "invalid-json", `{"Adapters":[]}`, testCombinedAdapters} {
		t.Run(combined, func(t *testing.T) {
			var calls atomic.Int32
			raw, ipv4, ipv6 := collectSnapshotAdapterData(func(_ context.Context, _ string, args ...string) (string, error) {
				calls.Add(1)
				switch args[len(args)-1] {
				case snapshotAdaptersCommand:
					// Even apparently valid output must not mask a command failure.
					if combined == testCombinedAdapters {
						return combined, errors.New("partial query")
					}
					return combined, nil
				case snapshotAdapterBasicsCommand:
					return `{"Name":"WLAN","Status":"Up"}`, nil
				case adapterBindingCommand("ms_tcpip"):
					return "", errors.New("IPv4 query unavailable")
				case adapterBindingCommand("ms_tcpip6"):
					return `{"Name":"WLAN","Enabled":true}`, nil
				default:
					return "", errors.New("unexpected command")
				}
			})
			if calls.Load() != 4 || !strings.Contains(raw, "WLAN") || ipv4 != nil || !ipv6["WLAN"] {
				t.Fatalf("calls=%d, raw=%s, IPv4=%v IPv6=%v", calls.Load(), raw, ipv4, ipv6)
			}
		})
	}
}

func TestSnapshotAdapterParserEmptyAndMultipleAdapters(t *testing.T) {
	raw, ipv4, ipv6, err := parseSnapshotAdapterData(`{"Adapters":[],"Bindings":[]}`)
	if err != nil || raw != "[]" || len(ipv4) != 0 || len(ipv6) != 0 {
		t.Fatalf("empty adapters: %s %v %v %v", raw, ipv4, ipv6, err)
	}
	_, ipv4, ipv6, err = parseSnapshotAdapterData(`{"Adapters":[{"Name":"WLAN"},{"Name":"Ethernet"}],"Bindings":[{"Name":"WLAN","ComponentID":"ms_tcpip","Enabled":true},{"Name":"Ethernet","ComponentID":"ms_tcpip6","Enabled":false},{"Name":"","ComponentID":"ms_tcpip","Enabled":true},{"Name":"WLAN","ComponentID":"other","Enabled":true}]}`)
	if err != nil || !reflect.DeepEqual(ipv4, map[string]bool{"WLAN": true}) || !reflect.DeepEqual(ipv6, map[string]bool{"Ethernet": false}) {
		t.Fatalf("binding maps: %v %v %v", ipv4, ipv6, err)
	}
}

// Execute the production batching script with mock cmdlets only. No host
// adapter, protocol binding, route or proxy configuration is read or changed.
func TestSnapshotAdaptersPowerShellBatchWithMockCmdlets(t *testing.T) {
	harness := `
function Get-NetAdapter {
    [pscustomobject]@{Name='测试网卡';Status='Up';MacAddress='00-11-22-33-44-55';InterfaceDescription='Wireless'}
}
function Get-NetAdapterBinding {
    param([string[]]$ComponentID)
    if($ComponentID.Count -ne 2 -or $ComponentID[0] -ne 'ms_tcpip' -or $ComponentID[1] -ne 'ms_tcpip6'){throw 'unexpected binding query'}
    [pscustomobject]@{Name='测试网卡';ComponentID='ms_tcpip';Enabled=$false}
    [pscustomobject]@{Name='测试网卡';ComponentID='ms_tcpip6';Enabled=$true}
}
` + snapshotAdaptersCommand
	script := filepath.Join(t.TempDir(), "snapshot.ps1")
	if err := os.WriteFile(script, append([]byte{0xef, 0xbb, 0xbf}, []byte(harness)...), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := execWithTimeout(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
	if err != nil {
		t.Fatalf("mock PowerShell query: %v, %s", err, raw)
	}
	basics, ipv4, ipv6, err := parseSnapshotAdapterData(raw)
	if err != nil || !strings.Contains(basics, "测试网卡") || ipv4["测试网卡"] || !ipv6["测试网卡"] {
		t.Fatalf("mock PowerShell result: %s, %v", raw, err)
	}
}
