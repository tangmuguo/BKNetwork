package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

var errUnknownWarpAction = errors.New("unknown warp action")

func probeWarpStatus(ctx context.Context) warpSnapshot {
	result := warpSnapshot{CheckedAt: time.Now().Format(time.RFC3339)}
	out, err := execWithTimeout(ctx, "warp-cli", "--json", "status")
	result.Raw = strings.TrimSpace(out)
	if err != nil && result.Raw == "" {
		result.Error = err.Error()
		return result
	}
	if status, reason, ok := parseWarpJSONStatus(result.Raw); ok {
		result.Status = status
		result.Reason = reason
		result.Connected = strings.EqualFold(status, "connected")
		return result
	}
	result.Connected, result.Status = parseWarpConnected(result.Raw)
	return result
}

func parseWarpJSONStatus(raw string) (status, reason string, ok bool) {
	var payload struct {
		Status string          `json:"status"`
		Reason json.RawMessage `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return "", "", false
	}
	payload.Status = strings.TrimSpace(payload.Status)
	if payload.Status == "" {
		return "", "", false
	}
	return payload.Status, parseWarpJSONReason(payload.Reason), true
}

func parseWarpJSONReason(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	// Newer Cloudflare clients encode reasons such as SettingsChanged as an
	// object. The object payload can be very large; the top-level discriminator
	// is all the state machine needs.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for key := range object {
			return strings.TrimSpace(key)
		}
	}
	return strings.Trim(strings.TrimSpace(string(raw)), "\"")
}

func probeWarpSettings(ctx context.Context) warpSettingsSnapshot {
	result := warpSettingsSnapshot{CheckedAt: time.Now().Format(time.RFC3339)}
	out, err := execWithTimeout(ctx, "warp-cli", "settings")
	raw := strings.TrimSpace(out)
	if err != nil && raw == "" {
		result.Error = err.Error()
		return result
	}
	result.Mode = parseWarpSettingsValue(raw, "Mode")
	result.TunnelProtocol = parseWarpSettingsValue(raw, "WARP tunnel protocol")
	return result
}

func parseWarpSettingsValue(raw, key string) string {
	needle := key + ":"
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		idx := strings.Index(line, needle)
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(line[idx+len(needle):])
		if value != "" {
			return value
		}
	}
	return ""
}

func parseWarpConnected(raw string) (bool, string) {
	text := strings.ToLower(strings.TrimSpace(raw))
	if text == "" {
		return false, ""
	}
	if strings.Contains(text, "status update: connected") && (strings.Contains(text, "network: healthy") || strings.Contains(text, "network: unstable")) {
		return true, "Connected"
	}
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" || value == "" {
			continue
		}
		keyLower := strings.ToLower(key)
		if strings.Contains(keyLower, "status") || strings.Contains(keyLower, "state") {
			valueLower := strings.ToLower(value)
			if strings.Contains(valueLower, "connect") || strings.Contains(valueLower, "check") || strings.Contains(valueLower, "update") || strings.Contains(valueLower, "disabled") || strings.Contains(valueLower, "off") || strings.Contains(valueLower, "disconnect") {
				return false, value
			}
			return false, ""
		}
	}
	return false, ""
}

type warpPreflightSnapshot struct {
	OK                    bool     `json:"ok"`
	SelectedInterface     string   `json:"selectedInterface"`
	DetectedIPv6Interface string   `json:"detectedIPv6Interface,omitempty"`
	GlobalIPv6            []string `json:"globalIPv6"`
	Code                  string   `json:"code,omitempty"`
	Message               string   `json:"message,omitempty"`
	Raw                   string   `json:"raw,omitempty"`
}

// warpUnderlaySnapshot describes the physical network Cloudflare would use
// outside the encrypted tunnel. Free-flow mode must not be accepted while an
// IPv4 underlay is still visible, even when warp-cli already says Connected.
type warpUnderlaySnapshot struct {
	OK                    bool   `json:"ok"`
	SelectedInterface     string `json:"selectedInterface"`
	DetectedIPv4Interface string `json:"detectedIPv4Interface,omitempty"`
	DetectedIPv6Interface string `json:"detectedIPv6Interface,omitempty"`
	Code                  string `json:"code,omitempty"`
	Message               string `json:"message,omitempty"`
	Raw                   string `json:"raw,omitempty"`
}

func parseWarpDebugNetworkInterface(raw, family string) string {
	prefix := strings.ToLower(strings.TrimSpace(family)) + ":"
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if !strings.HasPrefix(strings.ToLower(line), prefix) {
			continue
		}
		open := strings.Index(line, "[")
		if open < 0 {
			return ""
		}
		rest := line[open+1:]
		if end := strings.Index(rest, ";"); end >= 0 {
			rest = rest[:end]
		} else if end := strings.Index(rest, "]"); end >= 0 {
			rest = rest[:end]
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

func evaluateWarpIPv6Underlay(raw, ifName string) warpUnderlaySnapshot {
	result := warpUnderlaySnapshot{
		SelectedInterface:     strings.TrimSpace(ifName),
		DetectedIPv4Interface: parseWarpDebugNetworkInterface(raw, "IPv4"),
		DetectedIPv6Interface: parseWarpDebugNetworkInterface(raw, "IPv6"),
		Raw:                   strings.TrimSpace(raw),
	}
	if result.DetectedIPv6Interface == "" {
		result.Code = "warp_no_ipv6_underlay"
		result.Message = "Cloudflare 尚未识别到可用的 IPv6 外层网络"
		return result
	}
	if !strings.EqualFold(result.DetectedIPv6Interface, result.SelectedInterface) {
		result.Code = "warp_ipv6_underlay_conflict"
		result.Message = fmt.Sprintf("Cloudflare 当前选择的 IPv6 外层网卡是 %s，而不是 %s", result.DetectedIPv6Interface, result.SelectedInterface)
		return result
	}
	if result.DetectedIPv4Interface != "" {
		result.Code = "warp_ipv4_underlay_still_available"
		result.Message = fmt.Sprintf("Cloudflare 仍能看到 IPv4 外层网卡 %s，继续连接可能被 Happy Eyeballs 选为 IPv4 路径", result.DetectedIPv4Interface)
		return result
	}
	result.OK = true
	return result
}

func probeWarpIPv6Underlay(ctx context.Context, ifName string) warpUnderlaySnapshot {
	out, err := execWithTimeout(ctx, "warp-cli", "debug", "network")
	result := evaluateWarpIPv6Underlay(out, ifName)
	if err != nil && strings.TrimSpace(out) == "" {
		result.Code = "warp_underlay_probe_failed"
		result.Message = fmt.Sprintf("无法读取 Cloudflare 外层网络状态：%v", err)
	}
	return result
}

func waitForWarpIPv6Underlay(ctx context.Context, ifName string) warpUnderlaySnapshot {
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	last := probeWarpIPv6Underlay(ctx, ifName)
	for {
		if last.OK {
			return last
		}
		select {
		case <-ctx.Done():
			if last.Message == "" {
				last.Code = "warp_underlay_timeout"
				last.Message = "等待 Cloudflare 刷新为仅 IPv6 外层网络超时"
			}
			return last
		case <-ticker.C:
			last = probeWarpIPv6Underlay(ctx, ifName)
		}
	}
}

func globalIPv6ForInterface(ifName string) ([]string, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for _, addr := range addrs {
		ip, _, parseErr := net.ParseCIDR(addr.String())
		if parseErr != nil || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		result = append(result, ip.String())
	}
	return result, nil
}

func probeWarpPreflight(ctx context.Context, ifName string) warpPreflightSnapshot {
	result := warpPreflightSnapshot{
		SelectedInterface: strings.TrimSpace(ifName),
		GlobalIPv6:        []string{},
	}
	if result.SelectedInterface == "" {
		result.Code = "missing_interface"
		result.Message = "请先选择实际连接校园网的物理网卡"
		return result
	}

	addresses, err := globalIPv6ForInterface(result.SelectedInterface)
	if err != nil {
		result.Code = "interface_unavailable"
		result.Message = fmt.Sprintf("找不到网卡 %s，请重新选择目标网卡", result.SelectedInterface)
		return result
	}
	result.GlobalIPv6 = addresses
	if len(addresses) == 0 {
		result.Code = "no_global_ipv6"
		result.Message = fmt.Sprintf("网卡 %s 没有可用的公网 IPv6 地址，请先确认校园网 IPv6 正常", result.SelectedInterface)
		return result
	}

	out, probeErr := execWithTimeout(ctx, "warp-cli", "debug", "network")
	result.Raw = strings.TrimSpace(out)
	if probeErr != nil && result.Raw == "" {
		result.Code = "warp_network_probe_failed"
		result.Message = "无法读取 Cloudflare 的网络识别结果，请确认 Cloudflare One Client 服务正在运行"
		return result
	}
	result.DetectedIPv6Interface = parseWarpDebugNetworkInterface(result.Raw, "IPv6")
	if result.DetectedIPv6Interface == "" {
		result.Code = "warp_no_ipv6_network"
		result.Message = "Cloudflare One Client 没有识别到可用的 IPv6 网络"
		return result
	}
	if !strings.EqualFold(result.DetectedIPv6Interface, result.SelectedInterface) {
		result.Code = "warp_interface_conflict"
		result.Message = fmt.Sprintf("Cloudflare 当前把 %s 识别为主 IPv6 网卡，而不是 %s。请先关闭该软件的 TUN/VPN 模式或禁用对应虚拟网卡后重试", result.DetectedIPv6Interface, result.SelectedInterface)
		return result
	}
	result.OK = true
	return result
}

const warpConnectedStableFor = 8 * time.Second
const warpTerminalFailureGrace = 4 * time.Second
const warpDisconnectedStableFor = 750 * time.Millisecond

type warpConnectionStability struct {
	connectedSince time.Time
}

func (s *warpConnectionStability) observe(snapshot warpSnapshot, now time.Time) bool {
	if !snapshot.Connected {
		s.connectedSince = time.Time{}
		return false
	}
	if s.connectedSince.IsZero() {
		s.connectedSince = now
	}
	return now.Sub(s.connectedSince) >= warpConnectedStableFor
}

func waitForWarpConnected(ctx context.Context, ifName string) warpSnapshot {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	startedAt := time.Now()
	stability := warpConnectionStability{}
	last := probeWarpStatus(ctx)
	for {
		if last.Connected {
			underlay := probeWarpIPv6Underlay(ctx, ifName)
			last.Underlay = &underlay
			if !underlay.OK {
				last.Connected = false
				last.Error = underlay.Message
			}
		}
		if stability.observe(last, time.Now()) {
			return last
		}
		// Unable/failed states require an explicit retry on some client builds.
		// Return this round to the orchestrator, which disconnects cleanly and
		// starts the next round without requiring another click from the user.
		if time.Since(startedAt) >= warpTerminalFailureGrace && warpStatusIsTerminalFailure(last) {
			return last
		}
		select {
		case <-ctx.Done():
			if last.Connected {
				last.Connected = false
				last.Error = fmt.Sprintf("WARP 曾报告已连接，但未连续稳定 %s", warpConnectedStableFor)
			} else if last.Error == "" {
				last.Error = "等待 WARP 连接超时"
			}
			return last
		case <-ticker.C:
			last = probeWarpStatus(ctx)
		}
	}
}

func waitForWarpDisconnected(ctx context.Context) warpSnapshot {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var disconnectedSince time.Time
	last := probeWarpStatus(ctx)
	for {
		status := normalizeWarpStatus(last.Status)
		if status == "disconnected" {
			if disconnectedSince.IsZero() {
				disconnectedSince = time.Now()
			}
			if time.Since(disconnectedSince) >= warpDisconnectedStableFor {
				return last
			}
		} else {
			disconnectedSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			if last.Error == "" {
				last.Error = "等待 WARP 完全断开超时"
			}
			return last
		case <-ticker.C:
			last = probeWarpStatus(ctx)
		}
	}
}

func warpStatusIsTerminalFailure(snapshot warpSnapshot) bool {
	status := normalizeWarpStatus(snapshot.Status)
	reason := strings.ToLower(strings.TrimSpace(snapshot.Reason))
	if strings.Contains(status, "unable") || strings.Contains(status, "error") || strings.Contains(status, "failed") {
		return true
	}
	return status == "disconnected" && reason != "" && reason != "manual"
}

func normalizeWarpStatus(status string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(status), "\"',; "))
}

func warpStatusIsInProgress(snapshot warpSnapshot) bool {
	if snapshot.Connected {
		return true
	}
	status := normalizeWarpStatus(snapshot.Status)
	return strings.Contains(status, "connecting") || strings.Contains(status, "checking") || strings.Contains(status, "configuring")
}

func applyWarpAction(ctx context.Context, action string) (string, error) {
	switch action {
	case "start":
		return execWithTimeout(ctx, "warp-cli", "connect")
	case "stop":
		return execWithTimeout(ctx, "warp-cli", "disconnect")
	default:
		return "", errUnknownWarpAction
	}
}

func normalizeWarpTunnelProtocol(protocol string) (string, error) {
	protocol = strings.TrimSpace(protocol)
	if strings.Contains(strings.ToLower(protocol), "wireguard") {
		return "WireGuard", nil
	}
	if strings.Contains(strings.ToLower(protocol), "masque") {
		return "MASQUE", nil
	}
	return "", fmt.Errorf("unsupported WARP tunnel protocol: %s", protocol)
}

func warpTunnelProtocolMatches(actual, expected string) bool {
	actualNormalized, actualErr := normalizeWarpTunnelProtocol(actual)
	expectedNormalized, expectedErr := normalizeWarpTunnelProtocol(expected)
	return actualErr == nil && expectedErr == nil && actualNormalized == expectedNormalized
}

func setWarpTunnelProtocol(ctx context.Context, protocol string) (string, error) {
	protocol, err := normalizeWarpTunnelProtocol(protocol)
	if err != nil {
		return "", err
	}
	return execWithTimeout(ctx, "warp-cli", "tunnel", "protocol", "set", protocol)
}

// setWarpTunnelProtocolVerified retries because the Cloudflare daemon may keep
// rejecting a protocol change for several seconds while an old tunnel is being
// torn down. A successful CLI exit is not enough: read settings back before
// continuing so the network stack is never switched on an unconfirmed protocol.
func setWarpTunnelProtocolVerified(ctx context.Context, protocol string) (string, error) {
	protocol, err := normalizeWarpTunnelProtocol(protocol)
	if err != nil {
		return "", err
	}

	var outputs []string
	var lastErr error
	for {
		out, setErr := setWarpTunnelProtocol(ctx, protocol)
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			outputs = append(outputs, trimmed)
		}
		if setErr != nil {
			lastErr = setErr
		} else {
			settings := probeWarpSettings(ctx)
			if warpTunnelProtocolMatches(settings.TunnelProtocol, protocol) {
				return strings.Join(outputs, "\n"), nil
			}
			if settings.Error != "" {
				lastErr = fmt.Errorf("协议命令已返回，但读取设置失败：%s", settings.Error)
			} else {
				lastErr = fmt.Errorf("协议命令已返回，但设置仍为 %q", settings.TunnelProtocol)
			}
		}

		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return strings.Join(outputs, "\n"), fmt.Errorf("无法确认 WARP 协议已切换到 %s：%w", protocol, lastErr)
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func StartWarp() error {
	if _, err := exec.LookPath("warp-cli"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
	defer cancel()
	_, err := applyWarpAction(ctx, "start")
	return err
}
