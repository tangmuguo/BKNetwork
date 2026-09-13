package handlers

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Keep recovery state on disk: closing BKNetwork does not stop its tunnel.
// Only IPv6 on the selected physical adapter is changed; keys are never read.
type homeRoutingState struct {
	TunnelName     string
	InterfaceName  string
	InterfaceGUID  string
	InterfaceIndex int
	Forwarding     bool
	WeakHostSend   bool
	NextHop        string
	Routes         []homeEndpointRoute
}

type homeEndpointRoute struct {
	DestinationPrefix string
	NextHop           string
	InterfaceIndex    int
	RouteMetric       int
}

type homeRoutingManager struct {
	statePath string
	run       func(context.Context, string, ...string) (string, error)
}

var homeInterfaceGUIDPattern = regexp.MustCompile(`(?i)^\{?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}?$`)

func newHomeRoutingManager() (*homeRoutingManager, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &homeRoutingManager{
		statePath: filepath.Join(dir, "BKNetwork", "home-routing.json"),
		run:       execWithTimeout,
	}, nil
}

func (m *homeRoutingManager) script(ctx context.Context, stage, body string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeoutApply)
	defer cancel()
	return m.run(commandCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"# BKNetwork home-routing: "+stage+"\n$ErrorActionPreference='Stop';[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false);"+body)
}

func (s *homeRoutingState) validate() error {
	if err := validateHomeTunnelName(s.TunnelName); err != nil {
		return err
	}
	if s.InterfaceIndex <= 0 || strings.TrimSpace(s.InterfaceName) == "" || !homeInterfaceGUIDPattern.MatchString(s.InterfaceGUID) || strings.HasPrefix(s.InterfaceGUID, "{") != strings.HasSuffix(s.InterfaceGUID, "}") {
		return fmt.Errorf("物理网卡恢复记录无效")
	}
	nextHop, err := netip.ParseAddr(s.NextHop)
	if err != nil || !nextHop.Is6() || nextHop.Is4In6() || nextHop.Zone() != "" {
		return fmt.Errorf("物理网卡 IPv6 网关无效")
	}
	for _, route := range s.Routes {
		prefix, err := netip.ParsePrefix(route.DestinationPrefix)
		if err != nil || prefix.Bits() != 128 || !validHomeEndpoint(prefix.Addr()) || route.InterfaceIndex != s.InterfaceIndex || route.NextHop != s.NextHop || (route.RouteMetric < 32768 || route.RouteMetric > 65535) {
			return fmt.Errorf("WireGuard 端点路由恢复记录无效")
		}
	}
	return nil
}

func (m *homeRoutingManager) load() (*homeRoutingState, error) {
	data, err := os.ReadFile(m.statePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state homeRoutingState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("读取 WireGuard 路由恢复记录失败：%w", err)
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

func (m *homeRoutingManager) save(state *homeRoutingState) error {
	if err := state.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(m.statePath), ".home-routing-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, m.statePath)
}

// An interface index can be reused after device removal. Match its GUID too.
func homeRoutingAdapterScript(state *homeRoutingState) string {
	return fmt.Sprintf("$a=@(Get-NetAdapter -IncludeHidden | Where-Object { $_.ifIndex -eq %d -and ([string]$_.InterfaceGuid).Trim('{}') -eq '%s' });if($a.Count -ne 1 -or -not $a[0].HardwareInterface){throw '物理网卡已变化，保留恢复记录，请检查原网卡'};",
		state.InterfaceIndex, strings.Trim(state.InterfaceGUID, "{}"))
}

func homeRoutingFlagsScript(state *homeRoutingState, forwarding, weakHostSend bool) string {
	f, w := "Disabled", "Disabled"
	if forwarding {
		f = "Enabled"
	}
	if weakHostSend {
		w = "Enabled"
	}
	return homeRoutingAdapterScript(state) + fmt.Sprintf(
		"Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv6 -PolicyStore ActiveStore -Forwarding %s -WeakHostSend %s -ErrorAction Stop;"+
			"$i=Get-NetIPInterface -InterfaceIndex %d -AddressFamily IPv6 -PolicyStore ActiveStore -ErrorAction Stop;"+
			"if([string]$i.Forwarding -ne '%s' -or [string]$i.WeakHostSend -ne '%s'){throw 'IPv6 转发/弱主机发送状态未达到目标值'};",
		state.InterfaceIndex, f, w, state.InterfaceIndex, f, w)
}

func (m *homeRoutingManager) checkOwnership(ifName, tunnelName string) error {
	state, err := m.load()
	if err != nil {
		return err
	}
	if state != nil && (!strings.EqualFold(state.TunnelName, tunnelName) || !strings.EqualFold(state.InterfaceName, ifName)) {
		return fmt.Errorf("请先停止 %s 并恢复网卡 %s，再切换家庭隧道或物理网卡", state.TunnelName, state.InterfaceName)
	}
	return nil
}

func (m *homeRoutingManager) prepare(ctx context.Context, ifName, tunnelName string) error {
	if err := validateHomeTunnelName(tunnelName); err != nil {
		return err
	}
	state, err := m.load()
	if err != nil {
		return err
	}
	if state != nil {
		if !strings.EqualFold(state.TunnelName, tunnelName) || !strings.EqualFold(state.InterfaceName, ifName) {
			return fmt.Errorf("请先停止 %s 并恢复网卡 %s，再切换家庭隧道或物理网卡", state.TunnelName, state.InterfaceName)
		}
	} else {
		name := escapePowerShellSingleQuotedString(ifName)
		body := fmt.Sprintf(
			"$a=@(Get-NetAdapter | Where-Object { $_.Name -eq '%s' });"+
				"if($a.Count -ne 1 -or -not $a[0].HardwareInterface -or [string]$a[0].Status -ne 'Up'){throw '必须选择已连接的物理网卡'};"+
				"$id=$a[0].ifIndex;$i=Get-NetIPInterface -InterfaceIndex $id -AddressFamily IPv6 -PolicyStore ActiveStore -ErrorAction Stop;"+
				"$r=Get-NetRoute -InterfaceIndex $id -AddressFamily IPv6 -DestinationPrefix '::/0' -PolicyStore ActiveStore -ErrorAction Stop | Sort-Object RouteMetric | Select-Object -First 1;"+
				"if($null -eq $r){throw '所选物理网卡没有 IPv6 默认网关'};"+
				"[pscustomobject]@{InterfaceName=$a[0].Name;InterfaceGUID=[string]$a[0].InterfaceGuid;InterfaceIndex=$id;Forwarding=([string]$i.Forwarding -eq 'Enabled');WeakHostSend=([string]$i.WeakHostSend -eq 'Enabled');NextHop=$r.NextHop} | ConvertTo-Json -Compress", name)
		raw, runErr := m.script(ctx, "snapshot", body)
		if runErr != nil {
			return fmt.Errorf("无法读取物理 IPv6 出口：%s：%w", strings.TrimSpace(raw), runErr)
		}
		state = &homeRoutingState{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), state); err != nil {
			return fmt.Errorf("无法解析物理 IPv6 出口：%w", err)
		}
		if !strings.EqualFold(state.InterfaceName, ifName) {
			return fmt.Errorf("返回的物理网卡与所选网卡不一致")
		}
		state.TunnelName = tunnelName
		// Persist BEFORE the first mutation, so even a crash can be recovered.
		if err := m.save(state); err != nil {
			return err
		}
	}
	out, err := m.script(ctx, "disable", homeRoutingFlagsScript(state, false, false))
	if err != nil {
		return fmt.Errorf("无法关闭物理 IPv6 出口的 Forwarding/WeakHostSend：%s：%w", strings.TrimSpace(out), err)
	}
	return nil
}

func validHomeEndpoint(addr netip.Addr) bool {
	return addr.IsValid() && addr.Is6() && !addr.Is4In6() && addr.Zone() == "" && addr.IsGlobalUnicast() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast()
}

func parseHomeWireGuardEndpoints(raw string) ([]netip.Addr, error) {
	var endpoints []netip.Addr
	seen := make(map[netip.Addr]bool)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("WireGuard 未返回有效的 IPv6 Endpoint")
		}
		endpoint, err := netip.ParseAddrPort(fields[1])
		if err != nil || endpoint.Port() == 0 || !validHomeEndpoint(endpoint.Addr()) {
			return nil, fmt.Errorf("家庭 WireGuard 必须使用带端口的 IPv6 Endpoint，当前值：%s", fields[1])
		}
		if !seen[endpoint.Addr()] {
			seen[endpoint.Addr()] = true
			endpoints = append(endpoints, endpoint.Addr())
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("WireGuard 没有可用的 IPv6 Endpoint")
	}
	return endpoints, nil
}

func homeEndpointRouteSelector(state *homeRoutingState, route homeEndpointRoute) string {
	return homeRoutingAdapterScript(state) + fmt.Sprintf(
		"$r=@(Get-NetRoute -InterfaceIndex %d -DestinationPrefix '%s' -PolicyStore ActiveStore -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -eq '%s' });",
		route.InterfaceIndex, route.DestinationPrefix, route.NextHop)
}

func (m *homeRoutingManager) protectEndpoints(ctx context.Context, tunnelName string, endpoints []netip.Addr) error {
	if len(endpoints) == 0 {
		return fmt.Errorf("WireGuard 没有可用的 IPv6 Endpoint")
	}
	for _, endpoint := range endpoints {
		if !validHomeEndpoint(endpoint) {
			return fmt.Errorf("WireGuard Endpoint 不是有效的 IPv6 地址")
		}
	}
	state, err := m.load()
	if err != nil {
		return err
	}
	if state == nil || !strings.EqualFold(state.TunnelName, tunnelName) {
		return fmt.Errorf("缺少该隧道的物理出口恢复记录")
	}
	// Recheck after interface creation, which may cause other components to
	// change forwarding. Host routes also protect against subsequent changes.
	if out, err := m.script(ctx, "disable", homeRoutingFlagsScript(state, false, false)); err != nil {
		return fmt.Errorf("隧道启动后无法确认物理 IPv6 出口：%s：%w", strings.TrimSpace(out), err)
	}
	for _, endpoint := range endpoints {
		route := homeEndpointRoute{DestinationPrefix: endpoint.String() + "/128", NextHop: state.NextHop, InterfaceIndex: state.InterfaceIndex}
		for _, prior := range state.Routes {
			if prior.DestinationPrefix == route.DestinationPrefix && prior.NextHop == route.NextHop && prior.InterfaceIndex == route.InterfaceIndex {
				route = prior
				break
			}
		}
		if route.RouteMetric == 0 {
			// A per-route ownership marker avoids deleting an ordinary metric-0
			// route created by another component during the check/add window.
			// Longest-prefix matching still prefers /128 over all /0 routes.
			var marker [2]byte
			if _, err := rand.Read(marker[:]); err != nil {
				return err
			}
			route.RouteMetric = 32768 + int(binary.LittleEndian.Uint16(marker[:])&32767)
		}
		selector := homeEndpointRouteSelector(state, route)
		raw, err := m.script(ctx, "route-exists", selector+"[bool]($r.Count -gt 0) | ConvertTo-Json -Compress")
		if err != nil {
			return fmt.Errorf("无法检查 Endpoint 路由：%s：%w", strings.TrimSpace(raw), err)
		}
		var exists bool
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &exists); err != nil {
			return fmt.Errorf("无法解析 Endpoint 路由状态：%w", err)
		}
		if !exists {
			owned := false
			for _, prior := range state.Routes {
				owned = owned || prior == route
			}
			if !owned {
				state.Routes = append(state.Routes, route)
				if err := m.save(state); err != nil {
					return err
				}
			}
			body := selector + fmt.Sprintf("if($r.Count -eq 0){New-NetRoute -InterfaceIndex %d -DestinationPrefix '%s' -NextHop '%s' -RouteMetric %d -PolicyStore ActiveStore -ErrorAction Stop | Out-Null};", route.InterfaceIndex, route.DestinationPrefix, route.NextHop, route.RouteMetric)
			if out, err := m.script(ctx, "route-add", body); err != nil {
				return fmt.Errorf("无法为 Endpoint 固定物理 IPv6 出口：%s：%w", strings.TrimSpace(out), err)
			}
		}
		body := homeRoutingAdapterScript(state) + fmt.Sprintf(
			"$best=Find-NetRoute -RemoteIPAddress '%s' -ErrorAction Stop | Where-Object { $null -ne $_.PSObject.Properties['DestinationPrefix'] } | Select-Object -First 1;"+
				"if($null -eq $best -or $best.InterfaceIndex -ne %d -or $best.NextHop -ne '%s'){throw 'Endpoint 仍未通过所选物理 IPv6 网关，可能存在竞争路由'};", endpoint.String(), state.InterfaceIndex, state.NextHop)
		if out, err := m.script(ctx, "route-verify", body); err != nil {
			return fmt.Errorf("Endpoint 外层路由校验失败：%s：%w", strings.TrimSpace(out), err)
		}
	}
	return nil
}

// Call only AFTER the tunnel is confirmed stopped. Keep state on any failure.
func (m *homeRoutingManager) restore(ctx context.Context, tunnelName string) error {
	state, err := m.load()
	if err != nil || state == nil {
		return err
	}
	if !strings.EqualFold(state.TunnelName, tunnelName) {
		return nil
	}
	for _, route := range state.Routes {
		body := homeEndpointRouteSelector(state, route) + fmt.Sprintf("$r | Where-Object RouteMetric -eq %d | Remove-NetRoute -Confirm:$false -ErrorAction Stop;", route.RouteMetric)
		if out, err := m.script(ctx, "route-remove", body); err != nil {
			return fmt.Errorf("清理临时 Endpoint 路由失败，已保留恢复记录：%s：%w", strings.TrimSpace(out), err)
		}
	}
	if out, err := m.script(ctx, "restore", homeRoutingFlagsScript(state, state.Forwarding, state.WeakHostSend)); err != nil {
		return fmt.Errorf("恢复物理 IPv6 网卡选项失败，已保留恢复记录：%s：%w", strings.TrimSpace(out), err)
	}
	return os.Remove(m.statePath)
}

// This error is only returned after stopHomeTunnel confirmed the service is
// stopped. Normal dual-stack access can be restored even if cleanup must retry.
type homeRoutingRestoreError struct{ err error }

func (e *homeRoutingRestoreError) Error() string { return e.err.Error() }
func (e *homeRoutingRestoreError) Unwrap() error { return e.err }
func homeTunnelStopConfirmed(err error) bool {
	if err == nil {
		return true
	}
	var restoreErr *homeRoutingRestoreError
	return errors.As(err, &restoreErr)
}

func restoreHomeOuterRouting(ctx context.Context, tunnelName string) error {
	manager, err := newHomeRoutingManager()
	if err == nil {
		err = manager.restore(ctx, tunnelName)
	}
	if err != nil {
		return &homeRoutingRestoreError{err: err}
	}
	return nil
}

func protectHomeTunnelEndpoints(ctx context.Context, manager *homeRoutingManager, tunnelName string) error {
	wgExe, err := resolveWGExecutable()
	if err != nil {
		return err
	}
	raw, err := execWithTimeout(ctx, wgExe, "show", tunnelName, "endpoints")
	if err != nil {
		return fmt.Errorf("无法读取 WireGuard Endpoint：%w", err)
	}
	endpoints, err := parseHomeWireGuardEndpoints(raw)
	if err != nil {
		return err
	}
	return manager.protectEndpoints(ctx, tunnelName, endpoints)
}

func homeTunnelRoutesScript(ifName, tunnelName string) string {
	return fmt.Sprintf("$ErrorActionPreference='Stop';"+
		"$physical=@(Get-NetAdapter | Where-Object Name -eq '%s');"+
		"if($physical.Count -ne 1 -or -not $physical[0].HardwareInterface -or [string]$physical[0].Status -ne 'Up'){throw '所选物理网卡不可用'};"+
		"$binding=@(Get-NetAdapterBinding -Name $physical[0].Name -ComponentID ms_tcpip,ms_tcpip6 -ErrorAction Stop);"+
		"$v4=@($binding | Where-Object ComponentID -eq 'ms_tcpip');$v6=@($binding | Where-Object ComponentID -eq 'ms_tcpip6');"+
		"if($v4.Count -ne 1 -or $v6.Count -ne 1 -or $v4[0].Enabled -or -not $v6[0].Enabled){throw '物理网卡没有保持仅 IPv6，不能确认校园 IPv6 免流'};"+
		"$t=@(Get-NetAdapter | Where-Object Name -eq '%s');"+
		"if($t.Count -ne 1 -or [string]$t[0].Status -ne 'Up'){throw 'WireGuard 网卡未运行'};"+
		"$prefixes=@(Get-NetRoute -InterfaceIndex $t[0].ifIndex -PolicyStore ActiveStore -ErrorAction Stop | Select-Object -ExpandProperty DestinationPrefix);"+
		"if(-not (($prefixes -contains '0.0.0.0/0') -or (($prefixes -contains '0.0.0.0/1') -and ($prefixes -contains '128.0.0.0/1')))){throw 'WireGuard 没有安装 IPv4 默认路由'};"+
		"if(-not (($prefixes -contains '::/0') -or (($prefixes -contains '::/1') -and ($prefixes -contains '8000::/1')))){throw 'WireGuard 没有安装 IPv6 默认路由'};"+
		"foreach($ip in @('1.1.1.1','2606:4700:4700::1111')){"+
		"$best=Find-NetRoute -RemoteIPAddress $ip -ErrorAction Stop | Where-Object { $null -ne $_.PSObject.Properties['DestinationPrefix'] } | Select-Object -First 1;"+
		"if($null -eq $best -or $best.InterfaceIndex -ne $t[0].ifIndex){throw '公网流量未使用所选 WireGuard 隧道，请检查竞争路由或其他 TUN 网卡'}};",
		escapePowerShellSingleQuotedString(ifName), escapePowerShellSingleQuotedString(tunnelName))
}

func verifyHomeTunnelRoutes(ctx context.Context, ifName, tunnelName string) error {
	commandCtx, cancel := context.WithTimeout(ctx, timeoutApply)
	defer cancel()
	out, err := execWithTimeout(commandCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", homeTunnelRoutesScript(ifName, tunnelName))
	if err != nil {
		return fmt.Errorf("%s：%w", strings.TrimSpace(out), err)
	}
	return nil
}
