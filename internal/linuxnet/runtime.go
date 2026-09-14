package linuxnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) probeWarp(ctx context.Context) WarpStatus {
	status := WarpStatus{Installed: m.commandAvailable(m.commands.warpCLI)}
	if !status.Installed {
		return status
	}
	output, err := m.run(ctx, m.commands.warpCLI, "--json", "status")
	if err != nil || strings.TrimSpace(output) == "" {
		output, err = m.run(ctx, m.commands.warpCLI, "status")
	}
	if strings.TrimSpace(output) == "" {
		if err != nil {
			status.Status = "unavailable"
		}
		return status
	}
	parsedStatus, connected := parseWarpStatus(output)
	status.Status = parsedStatus
	status.Connected = connected
	return status
}

func (m *Manager) preflightWarp(ctx context.Context) error {
	output, err := m.run(ctx, m.commands.warpCLI, "settings")
	if err != nil || strings.TrimSpace(output) == "" {
		return errors.New("无法读取 WARP 当前模式，请确认 warp-cli 服务已注册并运行；为避免 DNS-only 模式误报，已拒绝连接")
	}
	mode := parseWarpMode(output)
	if mode == "" {
		return errors.New("WARP 设置未返回 Mode，无法确认当前不是 DNS-only 模式")
	}
	if !strings.Contains(mode, "warp") {
		return fmt.Errorf("WARP 当前模式为 %s，请先运行 warp-cli mode warp+doh 再重试", mode)
	}
	return nil
}

// parseWarpMode accepts both the legacy settings output ("Mode: warp+doh")
// and current warp-cli output, which prefixes the setting source (for
// example, "(user set)\tMode: WarpWithDnsOverHttps").  The source prefix is
// informational; the value after the Mode label is what preflight needs.
func parseWarpMode(raw string) string {
	const label = "mode:"

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		index := strings.Index(lower, label)
		if index < 0 {
			continue
		}
		// Do not accidentally accept a different setting whose name merely
		// contains "mode".  A source prefix may precede the actual label, but
		// the character immediately before it must still be a separator.
		if index > 0 {
			previous := line[index-1]
			if (previous >= 'a' && previous <= 'z') || (previous >= 'A' && previous <= 'Z') || (previous >= '0' && previous <= '9') || previous == '_' {
				continue
			}
		}
		value := strings.TrimSpace(line[index+len(label):])
		if value != "" {
			return strings.ToLower(value)
		}
	}
	return ""
}

func parseWarpStatus(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	var payload struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil && strings.TrimSpace(payload.Status) != "" {
		value := strings.TrimSpace(payload.Status)
		return value, strings.EqualFold(value, "connected")
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.Contains(strings.ToLower(parts[0]), "status") {
			value := strings.TrimSpace(parts[1])
			return value, strings.EqualFold(value, "connected")
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "status update: connected") {
			return "Connected", true
		}
	}
	return "", false
}

func (m *Manager) probeWireGuard(ctx context.Context, preferred string) WireGuardStatus {
	status := WireGuardStatus{Installed: m.commandAvailable(m.commands.wg)}
	if !status.Installed {
		return status
	}
	output, err := m.run(ctx, m.commands.wg, "show", "interfaces")
	if err != nil {
		return status
	}
	interfaces := strings.Fields(output)
	if preferred != "" {
		if err := validateInterfaceName(preferred); err == nil {
			found := false
			for _, name := range interfaces {
				if name == preferred {
					found = true
					break
				}
			}
			if !found {
				interfaces = append(interfaces, preferred)
			}
		}
	}
	for _, name := range interfaces {
		if validateInterfaceName(name) != nil {
			continue
		}
		handshakeOutput, handshakeErr := m.run(ctx, m.commands.wg, "show", name, "latest-handshakes")
		if handshakeErr != nil {
			continue
		}
		latest, ok := parseLatestHandshake(handshakeOutput)
		if !ok {
			continue
		}
		if status.LatestHandshake == "" || latest.After(parseTime(status.LatestHandshake)) {
			status.LatestHandshake = latest.UTC().Format(time.RFC3339)
		}
		if time.Since(latest) <= handshakeFreshFor && !latest.After(time.Now().Add(30*time.Second)) {
			status.Connected = true
		}
	}
	return status
}

func parseLatestHandshake(raw string) (time.Time, bool) {
	var latest time.Time
	for _, field := range strings.Fields(raw) {
		seconds, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || seconds <= 0 {
			continue
		}
		stamp := time.Unix(seconds, 0)
		if latest.IsZero() || stamp.After(latest) {
			latest = stamp
		}
	}
	return latest, !latest.IsZero()
}

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func (m *Manager) checkConflicts(ctx context.Context, mode, selected string) error {
	if mode == "warp" {
		if wg := m.probeWireGuard(ctx, ""); wg.Connected {
			return errors.New("检测到已有 WireGuard 隧道正在握手，请先断开其他隧道再启动 WARP")
		}
	}
	if mode == "wireguard" {
		warp := m.probeWarp(ctx)
		if warp.Connected {
			return errors.New("检测到 WARP 已连接，请先断开 WARP 再启动 WireGuard")
		}
	}
	interfaces, err := m.collectInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("无法检查已有虚拟隧道：%w", err)
	}
	for _, iface := range interfaces {
		if iface.Name == selected || iface.Physical || iface.Name == "lo" || !interfaceStateUsable(iface.State) {
			continue
		}
		kind := strings.ToLower(iface.Type)
		name := strings.ToLower(iface.Name)
		if strings.Contains(kind, "wireguard") || strings.Contains(kind, "tun") || strings.Contains(kind, "tap") || strings.Contains(kind, "vpn") || strings.HasPrefix(name, "wg") || strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "tap") {
			return fmt.Errorf("检测到活动虚拟隧道 %s（%s），为避免覆盖其他连接请先断开它", iface.Name, iface.Type)
		}
	}
	return nil
}

func (m *Manager) captureUnderlay(ctx context.Context, ifName string, target Interface) (underlaySnapshot, error) {
	_ = target
	result := underlaySnapshot{Interface: ifName}
	// Only the selected uplink may be changed. Other usable physical IPv4
	// egress was rejected before reaching this function. We still read the
	// active profile when it currently has no lease: an automatic IPv4 method
	// could reacquire one while the tunnel is starting.
	output, err := m.run(ctx, m.commands.nmcli, "-t", "--escape", "no", "-f", "GENERAL.CONNECTION,GENERAL.CON-UUID,GENERAL.NM-MANAGED", "device", "show", ifName)
	if err != nil {
		return result, fmt.Errorf("无法读取网卡 %s 的 NetworkManager 连接：%w", ifName, err)
	}
	fields := parseKeyValueOutput(output)
	if strings.ToLower(fields["GENERAL.NM-MANAGED"]) != "yes" {
		return result, fmt.Errorf("网卡 %s 未由 NetworkManager 管理，拒绝修改其 IPv4 配置", ifName)
	}
	result.Connection = strings.TrimSpace(fields["GENERAL.CONNECTION"])
	result.ConnectionUUID = strings.TrimSpace(fields["GENERAL.CON-UUID"])
	if result.Connection == "" || result.Connection == "--" {
		return result, fmt.Errorf("网卡 %s 没有活动的 NetworkManager 连接", ifName)
	}
	connectionSelector := result.Connection
	if result.ConnectionUUID != "" && result.ConnectionUUID != "--" {
		connectionSelector = "uuid"
	}
	methodArgs := []string{"-t", "--escape", "no", "-g", "ipv4.method", "connection", "show"}
	if connectionSelector == "uuid" {
		methodArgs = append(methodArgs, "uuid", result.ConnectionUUID)
	} else {
		methodArgs = append(methodArgs, result.Connection)
	}
	methodOutput, err := m.run(ctx, m.commands.nmcli, methodArgs...)
	if err != nil {
		return result, fmt.Errorf("无法保存网卡 %s 的原 IPv4 方法：%w", ifName, err)
	}
	result.IPv4Method = strings.TrimSpace(strings.Split(strings.TrimSpace(methodOutput), "\n")[0])
	if result.IPv4Method == "" {
		return result, fmt.Errorf("无法识别网卡 %s 的原 IPv4 方法", ifName)
	}
	result.IPv4Disabled = result.IPv4Method != "disabled"
	return result, nil
}

func parseKeyValueOutput(raw string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		key, value, ok := strings.Cut(line, ":")
		if ok {
			result[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return result
}

func (m *Manager) disableIPv4(ctx context.Context, ifName string) error {
	if _, err := m.run(ctx, m.commands.nmcli, "device", "modify", ifName, "ipv4.method", "disabled"); err != nil {
		return fmt.Errorf("NetworkManager 禁用 IPv4 失败：%w", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		if m.noUsableIPv4(ctx, ifName) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("网卡 %s 仍保留 IPv4 地址", ifName)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待网卡 %s 释放 IPv4 超时：%w", ifName, ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (m *Manager) noUsableIPv4(ctx context.Context, ifName string) bool {
	output, err := m.run(ctx, m.commands.ip, "-4", "addr", "show", "dev", ifName)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field != "inet" || index+1 >= len(fields) {
				continue
			}
			ip := parseAddress(fields[index+1])
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return false
			}
		}
	}
	return true
}

func (m *Manager) restoreUnderlay(ctx context.Context, underlay underlaySnapshot) error {
	if !underlay.IPv4Disabled {
		return nil
	}
	if err := validateInterfaceName(underlay.Interface); err != nil {
		return fmt.Errorf("恢复原 IPv4 网卡名称无效：%w", err)
	}
	if underlay.IPv4Method == "" {
		return fmt.Errorf("没有保存网卡 %s 的原 IPv4 方法", underlay.Interface)
	}
	identityOutput, err := m.run(ctx, m.commands.nmcli, "-t", "--escape", "no", "-f", "GENERAL.CONNECTION,GENERAL.CON-UUID,GENERAL.NM-MANAGED", "device", "show", underlay.Interface)
	if err != nil {
		return fmt.Errorf("无法确认网卡 %s 仍使用原 NetworkManager 连接：%w", underlay.Interface, err)
	}
	identity := parseKeyValueOutput(identityOutput)
	if strings.ToLower(identity["GENERAL.NM-MANAGED"]) != "yes" {
		return fmt.Errorf("网卡 %s 当前未由 NetworkManager 管理，拒绝恢复未知连接", underlay.Interface)
	}
	if underlay.ConnectionUUID == "" || identity["GENERAL.CON-UUID"] != underlay.ConnectionUUID {
		return fmt.Errorf("网卡 %s 的 NetworkManager 连接 UUID 已变化（原 %s，当前 %s），拒绝覆盖新连接", underlay.Interface, underlay.ConnectionUUID, identity["GENERAL.CON-UUID"])
	}
	if _, err := m.run(ctx, m.commands.nmcli, "device", "modify", underlay.Interface, "ipv4.method", underlay.IPv4Method); err != nil {
		return fmt.Errorf("恢复网卡 %s 的 IPv4 方法 %q 失败：%w", underlay.Interface, underlay.IPv4Method, err)
	}
	return nil
}

func (m *Manager) waitForVerified(ctx context.Context, mode, physicalIfName, tunnelIfName string) (bool, string) {
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		verified, message := m.verifyRuntime(ctx, mode, physicalIfName, tunnelIfName)
		if verified {
			return true, ""
		}
		last = message
		select {
		case <-ctx.Done():
			if last == "" {
				last = "等待 IPv6 隧道验证超时"
			}
			return false, last
		case <-ticker.C:
		}
	}
}

func (m *Manager) verifyRuntime(ctx context.Context, mode, physicalIfName, tunnelIfName string) (bool, string) {
	if err := validateInterfaceName(physicalIfName); err != nil {
		return false, "未选择可验证的物理或隧道网卡"
	}
	if mode == "warp" {
		warp := m.probeWarp(ctx)
		if !warp.Connected {
			return false, fmt.Sprintf("WARP 状态仍为 %s", fallbackText(warp.Status, "未连接"))
		}
		if !noPhysicalIPv4Egress(m, ctx, physicalIfName) {
			return false, "仍检测到物理 IPv4 出口，拒绝把 WARP 连接认定为 IPv6 免流"
		}
		trace, err := m.curlTrace(ctx, "", true)
		if err != nil {
			return false, fmt.Sprintf("WARP IPv6 连通性验证失败：%v", err)
		}
		if value := traceValue(trace, "warp"); !strings.EqualFold(value, "on") {
			return false, fmt.Sprintf("Cloudflare trace 未确认 WARP 已接管（warp=%s）", fallbackText(value, "未知"))
		}
		trace4, err := m.curlTrace(ctx, "", false)
		if err != nil {
			return false, fmt.Sprintf("WARP IPv4-in-tunnel 联网验证失败：%v", err)
		}
		if value := traceValue(trace4, "warp"); !strings.EqualFold(value, "on") {
			return false, fmt.Sprintf("Cloudflare IPv4 trace 未确认 WARP 已接管（warp=%s）", fallbackText(value, "未知"))
		}
		return true, ""
	}
	if mode == "wireguard" {
		if err := validateInterfaceName(tunnelIfName); err != nil {
			return false, "WireGuard 隧道接口名称无效"
		}
		wg := m.probeWireGuard(ctx, tunnelIfName)
		if !wg.Connected {
			return false, "WireGuard 尚未完成最近握手"
		}
		if !m.wireGuardEndpointOnIPv6(ctx, tunnelIfName, physicalIfName) {
			return false, "WireGuard 对端当前不是可验证的公网 IPv6 Endpoint，拒绝使用 IPv4 外层"
		}
		if !m.fullTunnelRoutes(ctx, tunnelIfName) {
			return false, "WireGuard 尚未同时接管 IPv4 和 IPv6 默认路由"
		}
		if !noPhysicalIPv4Egress(m, ctx, physicalIfName) {
			return false, "仍检测到物理 IPv4 出口，拒绝把 WireGuard 连接认定为 IPv6 免流"
		}
		if _, err := m.curlTrace(ctx, tunnelIfName, false); err != nil {
			return false, fmt.Sprintf("WireGuard IPv4 隧道联网验证失败：%v", err)
		}
		if _, err := m.curlTrace(ctx, tunnelIfName, true); err != nil {
			return false, fmt.Sprintf("WireGuard IPv6 隧道联网验证失败：%v", err)
		}
		return true, ""
	}
	if !noPhysicalIPv4Egress(m, ctx, physicalIfName) {
		return false, "仍检测到物理 IPv4 出口，当前直连未达到 IPv6-only 条件"
	}
	if _, err := m.curlTrace(ctx, physicalIfName, true); err != nil {
		return false, fmt.Sprintf("直连 IPv6 联网验证失败：%v", err)
	}
	return true, ""
}

func noPhysicalIPv4Egress(m *Manager, ctx context.Context, selected string) bool {
	interfaces, err := m.collectInterfaces(ctx)
	if err != nil {
		return false
	}
	selectedInterface, found := findInterface(interfaces, selected)
	if !found || !selectedInterface.Physical || !interfaceStateUsable(selectedInterface.State) || !hasGlobalIPv6(selectedInterface.IPv6) {
		return false
	}
	if !m.noUsableIPv4(ctx, selected) {
		return false
	}
	for _, iface := range interfaces {
		if !iface.Physical || iface.Name == selected || !interfaceStateUsable(iface.State) {
			continue
		}
		if hasGlobalIPv6(iface.IPv6) {
			return false
		}
		for _, address := range iface.IPv4 {
			ip := parseAddress(address)
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return false
			}
		}
	}
	return true
}

func (m *Manager) wireGuardEndpointOnIPv6(ctx context.Context, tunnelIfName, physicalIfName string) bool {
	if validateInterfaceName(tunnelIfName) != nil || validateInterfaceName(physicalIfName) != nil {
		return false
	}
	output, err := m.run(ctx, m.commands.wg, "show", tunnelIfName, "endpoints")
	if err != nil {
		return false
	}
	endpointHosts := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || fields[1] == "(none)" {
			return false
		}
		if err := validatePublicIPv6Endpoint(fields[1]); err != nil {
			return false
		}
		host, _, splitErr := net.SplitHostPort(fields[1])
		if splitErr != nil {
			return false
		}
		endpointHosts[host] = struct{}{}
	}
	if len(endpointHosts) == 0 {
		return false
	}
	fwmarkOutput, fwmarkErr := m.run(ctx, m.commands.wg, "show", tunnelIfName, "fwmark")
	if fwmarkErr != nil {
		return false
	}
	fwmarkFields := strings.Fields(fwmarkOutput)
	if len(fwmarkFields) == 0 {
		return false
	}
	fwmark := strings.TrimSpace(fwmarkFields[0])
	markArgs := []string{}
	if fwmark != "" && fwmark != "0" && !strings.EqualFold(fwmark, "off") {
		if _, err := strconv.ParseUint(fwmark, 0, 32); err != nil {
			return false
		}
		markArgs = append(markArgs, "mark", fwmark)
	}
	for host := range endpointHosts {
		routeArgs := append([]string{"-6", "route", "get", host}, markArgs...)
		routeOutput, routeErr := m.run(ctx, m.commands.ip, routeArgs...)
		if routeErr != nil || !routeUsesInterface(routeOutput, physicalIfName) {
			return false
		}
	}
	return true
}

func (m *Manager) fullTunnelRoutes(ctx context.Context, ifName string) bool {
	route4, err4 := m.run(ctx, m.commands.ip, "-4", "route", "get", "1.1.1.1")
	route6, err6 := m.run(ctx, m.commands.ip, "-6", "route", "get", "2606:4700:4700::1111")
	if err4 != nil || err6 != nil {
		return false
	}
	return routeUsesInterface(route4, ifName) && routeUsesInterface(route6, ifName)
}

func routeUsesInterface(raw, ifName string) bool {
	fields := strings.Fields(raw)
	for index, field := range fields {
		if field == "dev" && index+1 < len(fields) && fields[index+1] == ifName {
			return true
		}
	}
	return false
}

func (m *Manager) curlTrace(ctx context.Context, ifName string, forceIPv6 bool) (string, error) {
	args := []string{"--disable", "--noproxy", "*", "--proxy", "", "-fsS", "--connect-timeout", "5"}
	if forceIPv6 {
		args = append(args, "-6")
	} else {
		args = append(args, "-4")
	}
	if ifName != "" {
		args = append(args, "--interface", ifName)
	}
	args = append(args, "https://www.cloudflare.com/cdn-cgi/trace")
	return m.run(ctx, m.commands.curl, args...)
}

func traceValue(raw, key string) string {
	for _, line := range strings.Split(raw, "\n") {
		keyPart, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.EqualFold(strings.TrimSpace(keyPart), key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
