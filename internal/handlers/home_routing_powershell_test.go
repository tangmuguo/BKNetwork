//go:build windows

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Execute the actual generated scripts with fake cmdlets. No NetTCPIP or
// NetAdapter cmdlet is allowed to reach the host's real network configuration.
func TestHomeRoutingPowerShellLifecycleAndFreeFlow(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell unavailable")
	}
	scripts := make(map[string]string)
	state := homeRoutingState{InterfaceName: "WLAN", InterfaceGUID: "00000000-0000-0000-0000-000000000001", InterfaceIndex: 14, Forwarding: true, WeakHostSend: true, NextHop: "fe80::1"}
	snapshot, _ := json.Marshal(state)
	manager := &homeRoutingManager{statePath: filepath.Join(t.TempDir(), "state.json")}
	manager.run = func(_ context.Context, _ string, args ...string) (string, error) {
		body := args[len(args)-1]
		stage := strings.TrimPrefix(strings.SplitN(body, "\n", 2)[0], "# BKNetwork home-routing: ")
		scripts[stage] = body
		switch stage {
		case "snapshot":
			return string(snapshot), nil
		case "route-exists":
			return "false", nil
		default:
			return "", nil
		}
	}
	ctx := context.Background()
	if err := manager.prepare(ctx, "WLAN", "home"); err != nil {
		t.Fatal(err)
	}
	if err := manager.protectEndpoints(ctx, "home", []netip.Addr{netip.MustParseAddr("2001:db8::10")}); err != nil {
		t.Fatal(err)
	}
	if err := manager.restore(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	scripts["tunnel-verify"] = homeTunnelRoutesScript("WLAN", "home")
	payload, _ := json.Marshal(scripts)
	encodedPayload := base64.StdEncoding.EncodeToString(payload)
	harness := fmt.Sprintf(offlineHomeRoutingHarness, encodedPayload)
	scriptPath := filepath.Join(t.TempDir(), "offline-routing.ps1")
	if err := os.WriteFile(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(harness)...), 0o600); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline PowerShell scenarios failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OFFLINE_ROUTING_SCENARIOS_OK") {
		t.Fatalf("missing scenario completion: %s", out)
	}
}

const offlineHomeRoutingHarness = `
$ErrorActionPreference='Stop'
[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false)
$scripts=[System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s')) | ConvertFrom-Json
$script:forward='Enabled';$script:weak='Enabled';$script:physicalV4=$false
$script:hardware=$true;$script:guid='00000000-0000-0000-0000-000000000001'
$script:publicIndex=50;$script:endpointIndex=14;$script:mutations=0
$script:routes=@(
	[pscustomobject]@{InterfaceIndex=14;DestinationPrefix='::/0';NextHop='fe80::1';RouteMetric=256},
	[pscustomobject]@{InterfaceIndex=50;DestinationPrefix='0.0.0.0/0';NextHop='0.0.0.0';RouteMetric=0},
	[pscustomobject]@{InterfaceIndex=50;DestinationPrefix='::/0';NextHop='::';RouteMetric=0},
	[pscustomobject]@{InterfaceIndex=14;DestinationPrefix='2001:db8::20/128';NextHop='fe80::2';RouteMetric=5}
)
function Get-NetAdapter {
	[CmdletBinding()]param([switch]$IncludeHidden)
	[pscustomobject]@{Name='WLAN';ifIndex=14;InterfaceGuid=$script:guid;HardwareInterface=$script:hardware;Status='Up'}
	[pscustomobject]@{Name='home';ifIndex=50;InterfaceGuid='00000000-0000-0000-0000-000000000002';HardwareInterface=$false;Status='Up'}
	[pscustomobject]@{Name='Mihomo';ifIndex=57;InterfaceGuid='00000000-0000-0000-0000-000000000003';HardwareInterface=$false;Status='Up'}
}
function Get-NetIPInterface {
	[CmdletBinding()]param($InterfaceIndex,$AddressFamily,$PolicyStore)
	if($InterfaceIndex -ne 14 -or $AddressFamily -ne 'IPv6' -or $PolicyStore -ne 'ActiveStore'){throw 'unscoped IP interface read'}
	[pscustomobject]@{Forwarding=$script:forward;WeakHostSend=$script:weak}
}
function Set-NetIPInterface {
	[CmdletBinding()]param($InterfaceIndex,$AddressFamily,$PolicyStore,$Forwarding,$WeakHostSend)
	if($InterfaceIndex -ne 14 -or $AddressFamily -ne 'IPv6' -or $PolicyStore -ne 'ActiveStore'){throw 'attempted to change IPv4, another adapter, or persistent settings'}
	$script:mutations++;$script:forward=[string]$Forwarding;$script:weak=[string]$WeakHostSend
}
function Get-NetRoute {
	[CmdletBinding()]param($InterfaceIndex,$DestinationPrefix,$AddressFamily,$PolicyStore)
	if($PolicyStore -ne 'ActiveStore'){throw 'unexpected policy store'}
	$script:routes | Where-Object { ($null -eq $InterfaceIndex -or $_.InterfaceIndex -eq $InterfaceIndex) -and ([string]::IsNullOrEmpty($DestinationPrefix) -or $_.DestinationPrefix -eq $DestinationPrefix) }
}
function New-NetRoute {
	[CmdletBinding()]param($InterfaceIndex,$DestinationPrefix,$NextHop,$RouteMetric,$PolicyStore)
	if($InterfaceIndex -ne 14 -or $DestinationPrefix -ne '2001:db8::10/128' -or $NextHop -ne 'fe80::1' -or ($RouteMetric -lt 32768 -or $RouteMetric -gt 65535) -or $PolicyStore -ne 'ActiveStore'){throw 'wrong outer route'}
	$script:mutations++
	$script:routes+=([pscustomobject]@{InterfaceIndex=$InterfaceIndex;DestinationPrefix=$DestinationPrefix;NextHop=$NextHop;RouteMetric=$RouteMetric})
}
function Remove-NetRoute {
	[CmdletBinding(SupportsShouldProcess)]param([Parameter(ValueFromPipeline)]$InputObject)
	process {
		if($InputObject.DestinationPrefix -ne '2001:db8::10/128' -or $InputObject.InterfaceIndex -ne 14){throw 'attempted to remove pre-existing route'}
		$script:mutations++;$script:routes=@($script:routes | Where-Object { $_.DestinationPrefix -ne $InputObject.DestinationPrefix -or $_.InterfaceIndex -ne $InputObject.InterfaceIndex })
	}
}
function Get-NetAdapterBinding {
	[CmdletBinding()]param($Name,[string[]]$ComponentID)
	if($Name -ne 'WLAN'){throw 'inspected wrong physical adapter'}
	[pscustomobject]@{ComponentID='ms_tcpip';Enabled=$script:physicalV4}
	[pscustomobject]@{ComponentID='ms_tcpip6';Enabled=$true}
}
function Find-NetRoute {
	[CmdletBinding()]param($RemoteIPAddress)
	if($RemoteIPAddress -eq '2001:db8::10'){
		$index=$script:endpointIndex;$prefix='2001:db8::10/128';$hop='fe80::1'
	}else{
		$index=$script:publicIndex;$prefix='::/0';$hop='::'
	}
	[pscustomobject]@{IPAddress='2001:db8::100';InterfaceIndex=$index}
	[pscustomobject]@{DestinationPrefix=$prefix;InterfaceIndex=$index;NextHop=$hop}
}
function RunStage([string]$name){ & ([scriptblock]::Create([string]$scripts.$name)) }
function MustFail([string]$name){
	$caught=$false
	try { RunStage $name | Out-Null } catch { $caught=$true }
	if(-not $caught){throw ('expected rejection: '+$name)}
}
$snap=RunStage 'snapshot' | ConvertFrom-Json
if(-not $snap.Forwarding -or -not $snap.WeakHostSend -or $snap.InterfaceIndex -ne 14){throw 'wrong original snapshot'}
RunStage 'disable'
if($script:forward -ne 'Disabled' -or $script:weak -ne 'Disabled'){throw 'loop settings still active'}
if((RunStage 'route-exists' | ConvertFrom-Json)){throw 'route unexpectedly existed'}
RunStage 'route-add'
RunStage 'route-verify'
RunStage 'tunnel-verify'
$script:physicalV4=$true;MustFail 'tunnel-verify';$script:physicalV4=$false
$script:publicIndex=57;MustFail 'tunnel-verify';$script:publicIndex=50
$script:endpointIndex=57;MustFail 'route-verify';$script:endpointIndex=14
$originalRoutes=$script:routes
$script:routes=@($script:routes | Where-Object { $_.InterfaceIndex -ne 50 -or $_.DestinationPrefix -ne '0.0.0.0/0' })
MustFail 'tunnel-verify';$script:routes=$originalRoutes
$script:hardware=$false;MustFail 'snapshot';$script:hardware=$true
$before=$script:mutations;$script:guid='00000000-0000-0000-0000-000000000004'
MustFail 'restore'
if($before -ne $script:mutations){throw 'changed an adapter whose index was reused'}
$script:guid='00000000-0000-0000-0000-000000000001'
RunStage 'route-remove'
if(@($script:routes | Where-Object DestinationPrefix -eq '2001:db8::20/128').Count -ne 1){throw 'deleted pre-existing route'}
if(@($script:routes | Where-Object DestinationPrefix -eq '2001:db8::10/128').Count -ne 0){throw 'owned route remained'}
RunStage 'restore'
if($script:forward -ne 'Enabled' -or $script:weak -ne 'Enabled'){throw 'original flags were not restored'}
$script:routes+=([pscustomobject]@{InterfaceIndex=14;DestinationPrefix='2001:db8::10/128';NextHop='fe80::1';RouteMetric=0})
RunStage 'route-add'
RunStage 'route-remove'
if(@($script:routes | Where-Object { $_.DestinationPrefix -eq '2001:db8::10/128' -and $_.RouteMetric -eq 0 }).Count -ne 1){throw 'deleted route inserted by an external process in the check/add window'}
'OFFLINE_ROUTING_SCENARIOS_OK'
`
