package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bknetwork/internal/events"
	appsettings "bknetwork/internal/settings"
)

const (
	homeHandshakeFreshFor = 3 * time.Minute
	homeConnectTimeout    = 30 * time.Second
	homeInternetTimeout   = 15 * time.Second
	// A healthy connectivity probe exchanges only a few KiB. A very large,
	// almost one-way increase is consistent with recursive encapsulation of
	// the IPv6 Endpoint. Abort before that can consume GiBs and stall Windows.
	homeRunawaySentDelta = 64 * 1024 * 1024
	homeRunawayRatio     = 256
)

const (
	homeDialFailureAccessDenied     = "windows-socket-access-denied"
	homeDialFailureResourceLimit    = "windows-socket-resource-exhausted"
	homeDialFailureAddressMissing   = "windows-source-address-unavailable"
	homeDialFailureTrafficSurge     = "wireguard-traffic-surge"
	homeDialFailureGuardUnavailable = "wireguard-traffic-guard-unavailable"
)

var (
	homeTunnelNamePattern = regexp.MustCompile(`^[A-Za-z0-9_=+.-]{1,32}$`)
	homeServiceStateRE    = regexp.MustCompile(`(?im)(?:state|状态)\s*:\s*([1-7])`)
)

// HomeNetworkHandler controls a WireGuard profile which has already been
// imported through the official WireGuard for Windows application.  It does
// not accept, read, or save a WireGuard private key.
func HomeNetworkHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, err := appsettings.Load()
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeoutMedium)
			defer cancel()
			profiles, profilesErr := listHomeTunnelProfiles()
			status := probePreferredHomeNetworkStatus(ctx, cfg.HomeTunnelName, profiles)
			selectedTunnelName := cfg.HomeTunnelName
			if status.Running && strings.TrimSpace(status.TunnelName) != "" {
				// Select an externally started imported profile as well, so the
				// warning is visible and the page's Stop action targets it.
				selectedTunnelName = status.TunnelName
			}
			result := map[string]interface{}{
				"ok":         true,
				"tunnelName": selectedTunnelName,
				"profiles":   profiles,
				"status":     status,
			}
			if profilesErr != nil && !os.IsNotExist(profilesErr) {
				result["profilesError"] = profilesErr.Error()
			}
			writeJSON(w, result, http.StatusOK)
		case http.MethodPost:
			var payload struct {
				Action     string `json:"action"`
				IfName     string `json:"ifName"`
				TunnelName string `json:"tunnelName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
				return
			}
			payload.Action = strings.ToLower(strings.TrimSpace(payload.Action))
			payload.IfName = strings.TrimSpace(payload.IfName)
			payload.TunnelName = strings.TrimSpace(payload.TunnelName)
			if payload.Action != "start" && payload.Action != "stop" {
				writeJSON(w, map[string]string{"error": "unknown action"}, http.StatusBadRequest)
				return
			}
			if !requireAdmin(w, hub, "home-network", payload) {
				return
			}

			tunnelModeMu.Lock()
			defer tunnelModeMu.Unlock()

			if payload.Action == "start" {
				handleHomeNetworkStart(w, hub, payload.IfName, payload.TunnelName)
				return
			}
			handleHomeNetworkStop(w, hub, payload.IfName, payload.TunnelName)
		default:
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
		}
	}
}

func handleHomeNetworkStart(w http.ResponseWriter, hub *events.Hub, ifName, tunnelName string) {
	if ifName == "" {
		writeJSON(w, map[string]string{"error": "请先选择实际连接校园网的物理网卡"}, http.StatusBadRequest)
		return
	}
	if err := validateHomeTunnelName(tunnelName); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	if err := verifyHomeNetworkPreflight(ifName); err != nil {
		writeJSON(w, map[string]string{"error": "家庭网络启动检查未通过", "detail": err.Error()}, http.StatusConflict)
		notify(hub, "home-network.error", "home network preflight failed", map[string]string{"detail": err.Error()})
		return
	}
	profiles, err := listHomeTunnelProfiles()
	if err != nil {
		writeJSON(w, map[string]string{"error": "未找到 WireGuard 的受保护配置目录", "detail": err.Error()}, http.StatusBadRequest)
		return
	}
	if !containsHomeTunnelProfile(profiles, tunnelName) {
		writeJSON(w, map[string]string{"error": "未找到所选家庭 WireGuard 配置", "detail": "请先在官方 WireGuard 客户端导入 .conf 文件，再回到 BKNetwork 点击刷新。BKNetwork 不会读取或保存私钥。"}, http.StatusBadRequest)
		return
	}
	routing, err := newHomeRoutingManager()
	if err == nil {
		err = routing.checkOwnership(ifName, tunnelName)
	}
	if err != nil {
		writeJSON(w, map[string]string{"error": "无法准备家庭 WireGuard 的物理出口", "detail": err.Error()}, http.StatusConflict)
		return
	}
	cfg, err := appsettings.Load()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	previousTunnelName := strings.TrimSpace(cfg.HomeTunnelName)
	if runtimeState := getFreeFlowRuntimeState(); runtimeState.Mode == "home" && previousTunnelName != "" && !strings.EqualFold(previousTunnelName, tunnelName) {
		writeJSON(w, map[string]string{"error": "当前家庭 WireGuard 隧道仍在运行，请先关闭后再切换配置"}, http.StatusConflict)
		return
	}
	cfg.HomeTunnelName = tunnelName
	if err := appsettings.Save(cfg); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	warpOut, warpErr := disconnectWarpForHomeNetwork()
	if warpErr != nil {
		writeJSON(w, map[string]interface{}{"error": "无法关闭 Cloudflare WARP，未修改网络", "detail": warpErr.Error(), "warpOutput": warpOut}, http.StatusBadGateway)
		notify(hub, "home-network.error", "failed to disconnect WARP", map[string]string{"detail": warpErr.Error()})
		return
	}
	clearFreeFlowRuntimeState("")
	fail := func(code int, message, detail string, result map[string]interface{}) {
		failHomeNetworkStart(w, hub, ifName, tunnelName, code, message, detail, result)
	}
	// Preserve campus free-flow: IPv4 is carried INSIDE the IPv6 tunnel.
	stackOut, stackErr := applyNetworkMode(ifName, "ipv6")
	if stackErr != nil {
		fail(http.StatusInternalServerError, "切换到仅 IPv6 失败", stackErr.Error(), map[string]interface{}{"stackOutput": stackOut})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), homeConnectTimeout)
	defer cancel()
	// Forwarding/WeakHostSend can make Windows ignore WireGuard's physical
	// interface selection. Disable them BEFORE WireGuard installs /0 routes.
	if err := routing.prepare(ctx, ifName, tunnelName); err != nil {
		fail(http.StatusBadGateway, "无法防止家庭 WireGuard 外层路由回环", err.Error(), nil)
		return
	}
	// Capture counters before starting the service when possible. A stopped or
	// not-yet-created tunnel normally has no readable counters, in which case a
	// zero baseline lets the first post-start check include the earliest burst.
	guardBaseline := homeWireGuardMetrics{}
	if existing, baselineErr := probeHomeWireGuardMetrics(ctx, tunnelName); baselineErr == nil {
		guardBaseline = existing
	}
	serviceOut, serviceErr := ensureHomeTunnelService(ctx, tunnelName)
	if serviceErr != nil {
		fail(http.StatusBadGateway, "无法启动家庭 WireGuard 隧道", serviceErr.Error(), map[string]interface{}{"serviceOutput": serviceOut, "stackOutput": stackOut})
		return
	}
	if _, err := waitForHomeTunnelRunning(ctx, tunnelName); err != nil {
		fail(http.StatusBadGateway, "家庭 WireGuard 隧道未能进入运行状态", err.Error(), map[string]interface{}{"serviceOutput": serviceOut})
		return
	}
	checkTrafficGuard := func(checkCtx context.Context, stage string) bool {
		failureClass, detail := probeHomeTrafficGuard(checkCtx, tunnelName, guardBaseline, stage)
		if failureClass == "" {
			return true
		}
		message := "无法启用家庭 WireGuard 异常流量保护"
		if failureClass == homeDialFailureTrafficSurge {
			message = "家庭 WireGuard 异常流量保护已触发"
		}
		fail(http.StatusBadGateway, message, detail, map[string]interface{}{"serviceOutput": serviceOut, "failureClass": failureClass})
		return false
	}
	if !checkTrafficGuard(ctx, "隧道启动后") {
		return
	}
	// Only public runtime Endpoint metadata is read. Host routes prevent the
	// encrypted IPv6 packets from being routed back into the full tunnel.
	if err := protectHomeTunnelEndpoints(ctx, routing, tunnelName); err != nil {
		fail(http.StatusBadGateway, "家庭 WireGuard 的 IPv6 外层路由校验失败", err.Error(), nil)
		return
	}
	if !checkTrafficGuard(ctx, "Endpoint 路由保护后") {
		return
	}
	allowedIPs, allowedErr := probeHomeWireGuardAllowedIPs(ctx, tunnelName)
	if allowedErr != nil {
		fail(http.StatusBadGateway, "无法验证家庭 WireGuard 路由", allowedErr.Error(), map[string]interface{}{"serviceOutput": serviceOut})
		return
	}
	coverage := classifyHomeAllowedIPs(allowedIPs)
	if !coverage.IPv4 || !coverage.IPv6 {
		missing := make([]string, 0, 2)
		if !coverage.IPv4 {
			missing = append(missing, "IPv4 默认路由")
		}
		if !coverage.IPv6 {
			missing = append(missing, "IPv6 默认路由")
		}
		detail := fmt.Sprintf("WireGuard 配置未覆盖%s。请在 [Peer] 中设置 AllowedIPs = 0.0.0.0/0, ::/0，并在 [Interface] 中设置隧道 DNS；隧道内 IPv4 仍通过校园 IPv6 外层传输", strings.Join(missing, "和"))
		fail(http.StatusConflict, "家庭 WireGuard 路由配置不完整", detail, map[string]interface{}{"allowedIPs": allowedIPs})
		return
	}
	if err := verifyHomeTunnelRoutes(ctx, ifName, tunnelName); err != nil {
		fail(http.StatusConflict, "家庭 WireGuard 的实际路由或 IPv6 免流条件未通过", err.Error(), nil)
		return
	}
	if !checkTrafficGuard(ctx, "默认路由校验后") {
		return
	}
	triggerHomeTunnelTraffic(ctx)
	status := waitForHomeNetworkConnected(ctx, tunnelName, guardBaseline)
	if !status.Connected {
		if status.Error == "" {
			status.Error = "在等待时间内没有收到 WireGuard 握手"
		}
		message := "家庭 WireGuard 未完成握手"
		if status.FailureClass == homeDialFailureTrafficSurge {
			message = "家庭 WireGuard 异常流量保护已触发"
		} else if status.FailureClass == homeDialFailureGuardUnavailable {
			message = "家庭 WireGuard 启动保护不可用"
		}
		fail(http.StatusBadGateway, message, status.Error, map[string]interface{}{"serviceOutput": serviceOut, "status": status})
		return
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), homeInternetTimeout)
	internet := probeHomeTunnelInternet(probeCtx, tunnelName)
	probeCancel()
	if !internet.OK {
		detail := fmt.Sprintf("WireGuard 已握手，但隧道内 IPv4 联网验证失败：%s", internet.Error)
		message := "家庭 WireGuard 已握手但无法联网"
		if strings.HasPrefix(internet.FailureClass, "windows-") {
			detail = fmt.Sprintf("WireGuard 已握手且路由校验已通过，但 Windows 本地 TCP 探测失败：%s", internet.Error)
		} else if internet.FailureClass == homeDialFailureTrafficSurge {
			detail = fmt.Sprintf("WireGuard 已握手，但启动保护检测到异常流量：%s", internet.Error)
			message = "家庭 WireGuard 异常流量保护已触发"
		} else if internet.FailureClass == homeDialFailureGuardUnavailable {
			detail = fmt.Sprintf("WireGuard 已握手，但异常流量保护无法继续可靠读取计数：%s", internet.Error)
			message = "家庭 WireGuard 启动保护不可用"
		}
		fail(http.StatusBadGateway, message, detail, map[string]interface{}{"internet": internet, "allowedIPs": allowedIPs, "serviceOutput": serviceOut, "status": status})
		return
	}
	// Recheck after connectivity tests; don't label physical IPv4 as free-flow.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), timeoutLong)
	verifyErr := protectHomeTunnelEndpoints(verifyCtx, routing, tunnelName)
	if verifyErr == nil {
		verifyErr = verifyHomeTunnelRoutes(verifyCtx, ifName, tunnelName)
	}
	verifyCancel()
	if verifyErr != nil {
		fail(http.StatusConflict, "家庭 WireGuard 联网后 IPv6 免流条件发生变化", verifyErr.Error(), nil)
		return
	}
	finalGuardCtx, finalGuardCancel := context.WithTimeout(context.Background(), timeoutMedium)
	finalGuardOK := checkTrafficGuard(finalGuardCtx, "最终状态确认后")
	finalGuardCancel()
	if !finalGuardOK {
		return
	}
	setFreeFlowRuntimeState("home", ifName)
	status.Managed = true
	status.ProtectionHealthy = true
	status.ProtectionError = ""
	result := map[string]interface{}{"ok": true, "enabled": true, "tunnelName": tunnelName, "warpOutput": warpOut, "stackOutput": stackOut, "serviceOutput": serviceOut, "allowedIPs": allowedIPs, "internet": internet, "status": status}
	writeJSON(w, result, http.StatusOK)
	notify(hub, "home-network.ok", "home WireGuard mode enabled", result)
}

func failHomeNetworkStart(w http.ResponseWriter, hub *events.Hub, ifName, tunnelName string, code int, message, detail string, result map[string]interface{}) {
	if result == nil {
		result = make(map[string]interface{})
	}
	stopOut, stopErr := stopHomeTunnelForRollback(tunnelName)
	var rollbackOut string
	var rollbackErr error
	// A live tunnel must retain its protected Endpoint routes and host settings.
	if homeTunnelStopConfirmed(stopErr) {
		clearFreeFlowRuntimeState("")
		rollbackOut, rollbackErr = applyNetworkMode(ifName, "both")
	}
	rolledBack := stopErr == nil && rollbackErr == nil
	if rolledBack {
		message += "，已恢复双栈"
	} else {
		message += "；自动恢复未完成"
		if stopErr != nil {
			detail += "；停止隧道/恢复物理出口失败：" + stopErr.Error()
		}
		if rollbackErr != nil {
			detail += "；恢复双栈失败：" + rollbackErr.Error()
		}
	}
	if homeTunnelStopConfirmed(stopErr) {
		if strings.TrimSpace(detail) != "" {
			detail += "；"
		}
		if stopErr == nil {
			detail += "本次 Endpoint 保护已随隧道停止清理"
		} else {
			detail += "隧道已停止，但 Endpoint 路由或物理出口清理可能不完整"
		}
		detail += "；请勿在官方 WireGuard 客户端直接连接，请排除故障后仍从 BKNetwork 开关启动"
	}
	result["error"], result["detail"] = message, detail
	result["stopOutput"], result["rollbackOutput"], result["rolledBack"] = stopOut, rollbackOut, rolledBack
	log.Printf("home WireGuard start failed: tunnel=%q message=%q detail=%q rolledBack=%t", tunnelName, message, detail, rolledBack)
	writeJSON(w, result, code)
	notify(hub, "home-network.error", "home WireGuard start failed", result)
}

func handleHomeNetworkStop(w http.ResponseWriter, hub *events.Hub, ifName, requestedTunnelName string) {
	cfg, err := appsettings.Load()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	runtimeState := getFreeFlowRuntimeState()
	tunnelName := requestedTunnelName
	if runtimeState.Mode == "home" && strings.TrimSpace(cfg.HomeTunnelName) != "" {
		tunnelName = strings.TrimSpace(cfg.HomeTunnelName)
	} else if tunnelName == "" {
		tunnelName = strings.TrimSpace(cfg.HomeTunnelName)
	}
	if tunnelName != "" {
		if err := validateHomeTunnelName(tunnelName); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
	}

	restoreIfName := strings.TrimSpace(ifName)
	if runtimeState.Mode == "home" && strings.TrimSpace(runtimeState.Interface) != "" {
		restoreIfName = strings.TrimSpace(runtimeState.Interface)
	}
	if manager, managerErr := newHomeRoutingManager(); managerErr == nil {
		if saved, loadErr := manager.load(); loadErr == nil && saved != nil && strings.EqualFold(saved.TunnelName, tunnelName) {
			restoreIfName = saved.InterfaceName
		}
	}
	var cleanupErr error
	stopOut := "没有已配置的家庭 WireGuard 隧道"
	if tunnelName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), homeConnectTimeout)
		stopOut, err = stopHomeTunnel(ctx, tunnelName)
		cancel()
		if err != nil && !homeTunnelStopConfirmed(err) {
			writeJSON(w, map[string]interface{}{"error": "关闭家庭 WireGuard 隧道失败", "detail": err.Error(), "serviceOutput": stopOut}, http.StatusBadGateway)
			notify(hub, "home-network.error", "failed to stop home WireGuard tunnel", map[string]string{"detail": err.Error()})
			return
		}
		cleanupErr = err
	}
	clearFreeFlowRuntimeState("")
	stackOut := ""
	if restoreIfName != "" {
		stackOut, err = applyNetworkMode(restoreIfName, "both")
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "家庭隧道已关闭，但恢复双栈失败", "detail": err.Error(), "serviceOutput": stopOut, "stackOutput": stackOut}, http.StatusInternalServerError)
			return
		}
	}
	if cleanupErr != nil {
		result := map[string]interface{}{"error": "隧道已停止并恢复普通双栈，但物理出口清理未完成；可再次点击关闭重试", "detail": cleanupErr.Error(), "enabled": false, "serviceOutput": stopOut, "stackOutput": stackOut}
		writeJSON(w, result, http.StatusBadGateway)
		notify(hub, "home-network.error", "home WireGuard cleanup needs retry", result)
		return
	}
	result := map[string]interface{}{"ok": true, "enabled": false, "tunnelName": tunnelName, "serviceOutput": stopOut, "stackOutput": stackOut}
	writeJSON(w, result, http.StatusOK)
	notify(hub, "home-network.ok", "home WireGuard mode disabled", result)
}

func validateHomeTunnelName(name string) error {
	if !homeTunnelNamePattern.MatchString(strings.TrimSpace(name)) {
		return fmt.Errorf("家庭隧道名称只能包含英文字母、数字、_、=、+、. 或 -，且长度不超过 32")
	}
	return nil
}

func verifyHomeNetworkPreflight(ifName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutMedium)
	portUsage, portErr := probeHomeTCPPortUsage(ctx)
	cancel()
	if portErr != nil {
		return fmt.Errorf("无法完成 Windows TCP 临时端口压力预检，已在修改网络前安全中止：%w", portErr)
	}
	pressure := portUsage.Used
	if portUsage.Allocations > pressure {
		pressure = portUsage.Allocations
	}
	if homeTCPPortPressureHigh(pressure, portUsage.Capacity) {
		return fmt.Errorf("Windows TCP 临时端口压力过高（IPv4 唯一端口/记录 %d/%d、BOUND/TIME_WAIT %d/%d；IPv6 唯一端口/记录 %d/%d、BOUND/TIME_WAIT %d/%d；每个地址族动态范围容量 %d）；系统可能仍处于端口耗尽状态。请先关闭异常隧道并等待连接释放，再从 BKNetwork 重试", portUsage.IPv4Used, portUsage.IPv4Allocations, portUsage.IPv4Bound, portUsage.IPv4TimeWait, portUsage.IPv6Used, portUsage.IPv6Allocations, portUsage.IPv6Bound, portUsage.IPv6TimeWait, portUsage.Capacity)
	}
	addresses, err := globalIPv6ForInterface(ifName)
	if err != nil {
		return fmt.Errorf("找不到网卡 %s，请重新选择目标网卡", ifName)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("网卡 %s 没有可用的公网 IPv6 地址；家庭服务器必须能从该 IPv6 外层访问", ifName)
	}
	if _, err := resolveWireGuardExecutable(); err != nil {
		return fmt.Errorf("未找到 WireGuard for Windows，请先安装官方客户端")
	}
	return nil
}

type homeTCPPortUsage struct {
	Used            int
	Allocations     int
	Bound           int
	TimeWait        int
	IPv4Used        int
	IPv4Allocations int
	IPv4Bound       int
	IPv4TimeWait    int
	IPv6Used        int
	IPv6Allocations int
	IPv6Bound       int
	IPv6TimeWait    int
	Capacity        int
}

func probeHomeTCPPortUsage(ctx context.Context) (homeTCPPortUsage, error) {
	script := "$ErrorActionPreference='Stop';" +
		"$s=Get-NetTCPSetting -SettingName Internet -ErrorAction Stop;" +
		"if($null -eq $s){throw '无法读取 TCP 动态端口范围'};" +
		"$start=[int]$s.DynamicPortRangeStartPort;$count=[int]$s.DynamicPortRangeNumberOfPorts;$end=$start+$count-1;" +
		"$connections=@(Get-NetTCPConnection -ErrorAction Stop | Where-Object { $_.LocalPort -ge $start -and $_.LocalPort -le $end -and [string]$_.State -ne 'Listen' });" +
		"$v4=@($connections | Where-Object { $_.LocalAddress -notmatch ':' });$v6=@($connections | Where-Object { $_.LocalAddress -match ':' });" +
		"$v4Used=@($v4 | Select-Object -ExpandProperty LocalPort -Unique).Count;$v6Used=@($v6 | Select-Object -ExpandProperty LocalPort -Unique).Count;" +
		"$v4Bound=@($v4 | Where-Object { [string]$_.State -eq 'Bound' }).Count;$v6Bound=@($v6 | Where-Object { [string]$_.State -eq 'Bound' }).Count;" +
		"$v4TimeWait=@($v4 | Where-Object { [string]$_.State -eq 'TimeWait' }).Count;$v6TimeWait=@($v6 | Where-Object { [string]$_.State -eq 'TimeWait' }).Count;" +
		"[pscustomobject]@{IPv4Used=$v4Used;IPv4Allocations=$v4.Count;IPv4Bound=$v4Bound;IPv4TimeWait=$v4TimeWait;IPv6Used=$v6Used;IPv6Allocations=$v6.Count;IPv6Bound=$v6Bound;IPv6TimeWait=$v6TimeWait;Capacity=$count} | ConvertTo-Json -Compress"
	out, err := execWithTimeout(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return homeTCPPortUsage{}, fmt.Errorf("读取 Windows TCP 临时端口占用失败：%s：%w", strings.TrimSpace(out), err)
	}
	return parseHomeTCPPortUsage(out)
}

func parseHomeTCPPortUsage(raw string) (homeTCPPortUsage, error) {
	var usage homeTCPPortUsage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &usage); err != nil {
		return homeTCPPortUsage{}, fmt.Errorf("解析 Windows TCP 临时端口占用失败：%w", err)
	}
	if usage.IPv4Used < 0 || usage.IPv4Allocations < 0 || usage.IPv4Bound < 0 || usage.IPv4TimeWait < 0 ||
		usage.IPv6Used < 0 || usage.IPv6Allocations < 0 || usage.IPv6Bound < 0 || usage.IPv6TimeWait < 0 || usage.Capacity <= 0 ||
		usage.IPv4Used > usage.Capacity || usage.IPv6Used > usage.Capacity || usage.IPv4Bound > usage.IPv4Allocations || usage.IPv4TimeWait > usage.IPv4Allocations ||
		usage.IPv6Bound > usage.IPv6Allocations || usage.IPv6TimeWait > usage.IPv6Allocations {
		return homeTCPPortUsage{}, fmt.Errorf("Windows TCP 临时端口统计无效")
	}
	usage.Used = max(usage.IPv4Used, usage.IPv6Used)
	usage.Allocations = max(usage.IPv4Allocations, usage.IPv6Allocations)
	usage.Bound = max(usage.IPv4Bound, usage.IPv6Bound)
	usage.TimeWait = max(usage.IPv4TimeWait, usage.IPv6TimeWait)
	return usage, nil
}

func homeTCPPortPressureHigh(used, capacity int) bool {
	return capacity > 0 && used >= 0 && int64(used)*100 >= int64(capacity)*85
}

func disconnectWarpForHomeNetwork() (string, error) {
	if _, err := exec.LookPath("warp-cli"); err != nil {
		return "WARP 未安装", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
	status := probeWarpStatus(ctx)
	cancel()
	if !status.Connected && !warpStatusIsInProgress(status) {
		return "WARP 已断开", nil
	}
	return disconnectWarpAndWait()
}

func stopConfiguredHomeTunnel() (string, error) {
	cfg, err := appsettings.Load()
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(cfg.HomeTunnelName)
	if name == "" {
		return "未配置家庭 WireGuard 隧道", nil
	}
	if err := validateHomeTunnelName(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), homeConnectTimeout)
	defer cancel()
	out, err := stopHomeTunnel(ctx, name)
	return out, err
}

func wireGuardConfigDirectory() string {
	programFiles := strings.TrimSpace(os.Getenv("ProgramFiles"))
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	return filepath.Join(programFiles, "WireGuard", "Data", "Configurations")
}

func listHomeTunnelProfiles() ([]string, error) {
	entries, err := os.ReadDir(wireGuardConfigDirectory())
	if err != nil {
		return []string{}, err
	}
	profiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".conf.dpapi") {
			continue
		}
		name = name[:len(name)-len(".conf.dpapi")]
		if validateHomeTunnelName(name) == nil {
			profiles = append(profiles, name)
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return strings.ToLower(profiles[i]) < strings.ToLower(profiles[j]) })
	return profiles, nil
}

func containsHomeTunnelProfile(profiles []string, name string) bool {
	for _, profile := range profiles {
		if strings.EqualFold(profile, name) {
			return true
		}
	}
	return false
}

func homeTunnelProfilePath(name string) (string, error) {
	profiles, err := listHomeTunnelProfiles()
	if err != nil {
		return "", err
	}
	for _, profile := range profiles {
		if strings.EqualFold(profile, name) {
			return filepath.Join(wireGuardConfigDirectory(), profile+".conf.dpapi"), nil
		}
	}
	return "", fmt.Errorf("WireGuard 配置 %s 不存在", name)
}

func resolveWireGuardExecutable() (string, error) {
	for _, candidate := range []string{"wireguard.exe", "wireguard"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	for _, path := range []string{
		filepath.Join(strings.TrimSpace(os.Getenv("ProgramFiles")), "WireGuard", "wireguard.exe"),
		`C:\Program Files\WireGuard\wireguard.exe`,
		`C:\Program Files (x86)\WireGuard\wireguard.exe`,
	} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("wireguard.exe not found")
}

func resolveWGExecutable() (string, error) {
	wireGuardExe, err := resolveWireGuardExecutable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(wireGuardExe), "wg.exe")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for _, candidate := range []string{"wg.exe", "wg"} {
		if path, lookErr := exec.LookPath(candidate); lookErr == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("wg.exe not found")
}

type homeServiceStatus struct {
	Exists bool
	State  string
}

func homeTunnelServiceName(tunnelName string) string {
	return "WireGuardTunnel$" + strings.TrimSpace(tunnelName)
}

func queryHomeTunnelService(ctx context.Context, tunnelName string) (homeServiceStatus, error) {
	out, err := execWithTimeout(ctx, "sc.exe", "query", homeTunnelServiceName(tunnelName))
	state := parseHomeServiceState(out)
	if state != "" {
		return homeServiceStatus{Exists: true, State: state}, nil
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, "1060") || strings.Contains(lower, "does not exist") || strings.Contains(out, "不存在") {
		return homeServiceStatus{}, nil
	}
	if err != nil {
		return homeServiceStatus{}, fmt.Errorf("查询 WireGuard 服务失败：%w", err)
	}
	return homeServiceStatus{Exists: true, State: "unknown"}, nil
}

func parseHomeServiceState(raw string) string {
	match := homeServiceStateRE.FindStringSubmatch(raw)
	if len(match) == 2 {
		switch match[1] {
		case "1":
			return "stopped"
		case "2":
			return "start-pending"
		case "3":
			return "stop-pending"
		case "4":
			return "running"
		case "5":
			return "continue-pending"
		case "6":
			return "pause-pending"
		case "7":
			return "paused"
		}
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "running") || strings.Contains(raw, "正在运行") || strings.Contains(raw, "运行中"):
		return "running"
	case strings.Contains(lower, "stopped") || strings.Contains(raw, "已停止"):
		return "stopped"
	}
	return ""
}

func ensureHomeTunnelService(ctx context.Context, tunnelName string) (string, error) {
	service, err := queryHomeTunnelService(ctx, tunnelName)
	if err != nil {
		return "", err
	}
	if service.Exists {
		if service.State == "running" || service.State == "start-pending" {
			return "WireGuard 服务已在运行", nil
		}
		out, startErr := execWithTimeout(ctx, "sc.exe", "start", homeTunnelServiceName(tunnelName))
		if startErr != nil {
			return out, fmt.Errorf("启动 WireGuard 服务失败：%w", startErr)
		}
		return out, nil
	}

	configPath, err := homeTunnelProfilePath(tunnelName)
	if err != nil {
		return "", err
	}
	wireGuardExe, err := resolveWireGuardExecutable()
	if err != nil {
		return "", err
	}
	out, installErr := execWithTimeout(ctx, wireGuardExe, "/installtunnelservice", configPath)
	if installErr != nil {
		return out, fmt.Errorf("安装 WireGuard 隧道服务失败：%w", installErr)
	}

	// WireGuard creates a tunnel service as automatic by default. BKNetwork is
	// an on-demand switch, so change it to demand start after the first start.
	configOut, configErr := execWithTimeout(ctx, "sc.exe", "config", homeTunnelServiceName(tunnelName), "start=", "demand")
	if configErr != nil {
		return strings.TrimSpace(out + "\n" + configOut), fmt.Errorf("无法将 WireGuard 隧道设为按需启动：%w", configErr)
	}
	return strings.TrimSpace(out + "\n" + configOut), nil
}

func waitForHomeTunnelRunning(ctx context.Context, tunnelName string) (homeServiceStatus, error) {
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	var last homeServiceStatus
	for {
		status, err := queryHomeTunnelService(ctx, tunnelName)
		if err != nil {
			return status, err
		}
		last = status
		if status.Exists && status.State == "running" {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("等待 WireGuard 服务运行超时（最后状态：%s）", last.State)
		case <-ticker.C:
		}
	}
}

func stopHomeTunnel(ctx context.Context, tunnelName string) (string, error) {
	service, err := queryHomeTunnelService(ctx, tunnelName)
	if err != nil {
		return "", err
	}
	if !service.Exists || service.State == "stopped" {
		return "WireGuard 服务已停止", restoreHomeOuterRouting(ctx, tunnelName)
	}
	out, stopErr := execWithTimeout(ctx, "sc.exe", "stop", homeTunnelServiceName(tunnelName))
	if stopErr != nil {
		// The authoritative state is checked below; sc reports a non-zero result
		// when a tunnel completed stopping while the command was in flight.
		out = strings.TrimSpace(out + "\n" + stopErr.Error())
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, queryErr := queryHomeTunnelService(ctx, tunnelName)
		if queryErr != nil {
			return out, queryErr
		}
		if !current.Exists || current.State == "stopped" {
			return out, restoreHomeOuterRouting(ctx, tunnelName)
		}
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("等待 WireGuard 服务停止超时（最后状态：%s）", current.State)
		case <-ticker.C:
		}
	}
}

func stopHomeTunnelForRollback(tunnelName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), homeConnectTimeout)
	defer cancel()
	return stopHomeTunnel(ctx, tunnelName)
}

type homeWireGuardMetrics struct {
	HandshakeAt   time.Time
	ReceivedBytes uint64
	SentBytes     uint64
	PeerCount     int
}

// Query only peer metadata. `wg show <interface> dump` includes the interface
// private key and must not be used by BKNetwork.
func probeHomeWireGuardMetrics(ctx context.Context, tunnelName string) (homeWireGuardMetrics, error) {
	wgExe, err := resolveWGExecutable()
	if err != nil {
		return homeWireGuardMetrics{}, err
	}
	handshakes, handshakeErr := execWithTimeout(ctx, wgExe, "show", tunnelName, "latest-handshakes")
	if handshakeErr != nil && strings.TrimSpace(handshakes) == "" {
		return homeWireGuardMetrics{}, handshakeErr
	}
	transfers, transferErr := execWithTimeout(ctx, wgExe, "show", tunnelName, "transfer")
	if transferErr != nil && strings.TrimSpace(transfers) == "" {
		return homeWireGuardMetrics{}, transferErr
	}
	metrics, parseErr := parseHomeWireGuardMetrics(handshakes, transfers)
	if parseErr != nil {
		return homeWireGuardMetrics{}, parseErr
	}
	return metrics, nil
}

func parseHomeWireGuardDump(raw string) (homeWireGuardMetrics, error) {
	var metrics homeWireGuardMetrics
	hasPeer := false
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 8 {
			continue
		}
		hasPeer = true
		metrics.PeerCount++
		handshakeSeconds, _ := strconv.ParseInt(fields[4], 10, 64)
		if handshakeSeconds > 0 {
			handshake := time.Unix(handshakeSeconds, 0)
			if handshake.After(metrics.HandshakeAt) {
				metrics.HandshakeAt = handshake
			}
		}
		rx, _ := strconv.ParseUint(fields[5], 10, 64)
		tx, _ := strconv.ParseUint(fields[6], 10, 64)
		metrics.ReceivedBytes += rx
		metrics.SentBytes += tx
	}
	if !hasPeer {
		return metrics, fmt.Errorf("WireGuard 未返回 peer 状态")
	}
	return metrics, nil
}

func parseHomeWireGuardMetrics(handshakesRaw, transfersRaw string) (homeWireGuardMetrics, error) {
	var metrics homeWireGuardMetrics
	peers := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(handshakesRaw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		peers[fields[0]] = struct{}{}
		handshakeSeconds, _ := strconv.ParseInt(fields[1], 10, 64)
		if handshakeSeconds > 0 {
			handshake := time.Unix(handshakeSeconds, 0)
			if handshake.After(metrics.HandshakeAt) {
				metrics.HandshakeAt = handshake
			}
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(transfersRaw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		peers[fields[0]] = struct{}{}
		rx, _ := strconv.ParseUint(fields[1], 10, 64)
		tx, _ := strconv.ParseUint(fields[2], 10, 64)
		metrics.ReceivedBytes += rx
		metrics.SentBytes += tx
	}
	if len(peers) == 0 {
		return metrics, fmt.Errorf("WireGuard 未返回 peer 状态")
	}
	metrics.PeerCount = len(peers)
	return metrics, nil
}

func probeHomeWireGuardAllowedIPs(ctx context.Context, tunnelName string) ([]string, error) {
	wgExe, err := resolveWGExecutable()
	if err != nil {
		return nil, err
	}
	out, runErr := execWithTimeout(ctx, wgExe, "show", tunnelName, "allowed-ips")
	if runErr != nil && strings.TrimSpace(out) == "" {
		return nil, runErr
	}
	return parseHomeWireGuardAllowedIPs(out)
}

func parseHomeWireGuardAllowedIPs(raw string) ([]string, error) {
	allowed := make([]string, 0)
	hasPeer := false
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		hasPeer = true
		for _, field := range fields[1:] {
			for _, value := range strings.Split(field, ",") {
				value = strings.TrimSpace(value)
				if value != "" && value != "(none)" {
					allowed = append(allowed, value)
				}
			}
		}
	}
	if !hasPeer {
		return nil, fmt.Errorf("WireGuard 未返回 peer 的 AllowedIPs")
	}
	return allowed, nil
}

type homeAllowedIPCoverage struct {
	IPv4 bool
	IPv6 bool
}

func classifyHomeAllowedIPs(allowedIPs []string) homeAllowedIPCoverage {
	prefixes := make(map[string]bool)
	for _, raw := range allowedIPs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		prefixes[prefix.Masked().String()] = true
	}
	return homeAllowedIPCoverage{
		IPv4: prefixes["0.0.0.0/0"] || (prefixes["0.0.0.0/1"] && prefixes["128.0.0.0/1"]),
		IPv6: prefixes["::/0"] || (prefixes["::/1"] && prefixes["8000::/1"]),
	}
}

type homeInternetAttempt struct {
	Target       string `json:"target"`
	OK           bool   `json:"ok"`
	FailureClass string `json:"failureClass,omitempty"`
	Error        string `json:"error,omitempty"`
}

type homeInternetProbe struct {
	OK                     bool                  `json:"ok"`
	IPv4OK                 bool                  `json:"ipv4Ok"`
	DNSOK                  bool                  `json:"dnsOk"`
	Target                 string                `json:"target,omitempty"`
	Attempts               []homeInternetAttempt `json:"attempts,omitempty"`
	SourceAddress          string                `json:"sourceAddress,omitempty"`
	DNSName                string                `json:"dnsName"`
	WireGuardMetricsOK     bool                  `json:"wireGuardMetricsOk"`
	WireGuardReceivedDelta uint64                `json:"wireGuardReceivedDelta"`
	WireGuardSentDelta     uint64                `json:"wireGuardSentDelta"`
	FailureClass           string                `json:"failureClass,omitempty"`
	MetricsError           string                `json:"metricsError,omitempty"`
	Error                  string                `json:"error,omitempty"`
}

func findHomeTunnelIPv4(tunnelName string) (net.IP, error) {
	iface, err := net.InterfaceByName(tunnelName)
	if err != nil {
		interfaces, listErr := net.Interfaces()
		if listErr != nil {
			return nil, err
		}
		for i := range interfaces {
			if strings.EqualFold(strings.TrimSpace(interfaces[i].Name), strings.TrimSpace(tunnelName)) {
				iface = &interfaces[i]
				break
			}
		}
	}
	if iface == nil {
		return nil, fmt.Errorf("找不到 WireGuard 网卡 %s", tunnelName)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.To4(), nil
		}
	}
	return nil, fmt.Errorf("WireGuard 网卡 %s 没有 IPv4 地址；请在 [Interface] 中配置 Address = 10.66.66.x/32", tunnelName)
}

func homeTunnelIPv4ProbeTargets() []string {
	return []string{
		"www.cloudflare.com:443",
		"8.8.8.8:53",
		"9.9.9.9:53",
	}
}

func probeHomeTunnelInternet(ctx context.Context, tunnelName string) homeInternetProbe {
	targets := homeTunnelIPv4ProbeTargets()
	result := homeInternetProbe{DNSName: "www.msftconnecttest.com"}
	if len(targets) == 0 {
		result.Error = "未配置 IPv4 联网探测目标"
		return result
	}
	result.Target = targets[0]

	beforeMetrics, beforeMetricsErr := probeHomeWireGuardMetrics(ctx, tunnelName)
	if beforeMetricsErr != nil {
		result.FailureClass = homeDialFailureGuardUnavailable
		result.MetricsError = "探测前读取失败：" + beforeMetricsErr.Error()
		result.Error = "无法读取 WireGuard 公开流量计数，不能安全执行联网探测；已中止并回滚"
		return result
	}
	if failureClass, detail := evaluateHomeTrafficGuard(beforeMetrics, beforeMetrics, "联网探测前"); failureClass != "" {
		result.FailureClass = failureClass
		result.Error = detail
		return result
	}
	result.WireGuardMetricsOK = true
	checkTraffic := func(stage string) bool {
		current, err := probeHomeWireGuardMetrics(ctx, tunnelName)
		if err != nil {
			result.WireGuardMetricsOK = false
			result.FailureClass = homeDialFailureGuardUnavailable
			result.MetricsError = stage + "读取失败：" + err.Error()
			result.Error = "联网探测期间无法继续读取 WireGuard 公开流量计数；已安全中止并回滚"
			return false
		}
		result.WireGuardReceivedDelta = homeCounterDelta(beforeMetrics.ReceivedBytes, current.ReceivedBytes)
		result.WireGuardSentDelta = homeCounterDelta(beforeMetrics.SentBytes, current.SentBytes)
		failureClass, detail := evaluateHomeTrafficGuard(beforeMetrics, current, stage)
		if failureClass != "" {
			result.FailureClass = failureClass
			result.Error = detail
			return false
		}
		return true
	}

	sourceIP, err := findHomeTunnelIPv4(tunnelName)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.SourceAddress = sourceIP.String()

	for _, target := range targets {
		attempt := homeInternetAttempt{Target: target}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeoutDial)
		dialer := net.Dialer{Timeout: timeoutDial, LocalAddr: &net.TCPAddr{IP: sourceIP}}
		conn, dialErr := dialer.DialContext(attemptCtx, "tcp4", target)
		attemptCancel()
		if dialErr != nil {
			attempt.FailureClass = classifyHomeDialFailure(dialErr)
			attempt.Error = compactHomeDialError(dialErr)
			result.Attempts = append(result.Attempts, attempt)
			if !checkTraffic("TCP 探测后") {
				return result
			}
			continue
		}
		attempt.OK = true
		result.Attempts = append(result.Attempts, attempt)
		result.Target = target
		result.IPv4OK = true
		_ = conn.Close()
		if !checkTraffic("TCP 探测后") {
			return result
		}
		break
	}

	if !result.IPv4OK {
		failures := make([]string, 0, len(result.Attempts))
		for _, attempt := range result.Attempts {
			failures = append(failures, fmt.Sprintf("%s=%s", attempt.Target, attempt.Error))
		}
		flowDetail := ""
		if result.WireGuardMetricsOK {
			flowDetail = fmt.Sprintf("；探测期间 WireGuard 发送 +%d B、接收 +%d B，%s", result.WireGuardSentDelta, result.WireGuardReceivedDelta, describeHomeProbeTraffic(result.WireGuardSentDelta, result.WireGuardReceivedDelta))
		}
		result.FailureClass = commonHomeDialFailureClass(result.Attempts)
		if result.FailureClass != "" {
			result.Error = fmt.Sprintf("%s；从隧道地址 %s 发起的目标探测均未能创建连接（%s）%s。该结果不能单独证明 WireGuard 或 Ubuntu 转发失败；请检查 Windows System/Tcpip 事件 4231/4227、TIME_WAIT/BOUND 数量、动态端口及安全过滤规则", describeHomeDialFailure(result.FailureClass), result.SourceAddress, strings.Join(failures, "，"), flowDetail)
		} else {
			result.Error = fmt.Sprintf("从 %s 测试多个 IPv4 目标均失败（%s）%s", result.SourceAddress, strings.Join(failures, "，"), flowDetail)
		}
		return result
	}

	_, resolveErr := net.DefaultResolver.LookupHost(ctx, result.DNSName)
	if !checkTraffic("DNS 探测后") {
		return result
	}
	if resolveErr != nil {
		result.Error = fmt.Sprintf("隧道内 IPv4 已通，但 Windows DNS 解析失败：%v；请在 WireGuard [Interface] 中设置 DNS", resolveErr)
		return result
	}
	result.DNSOK = true
	result.OK = true
	return result
}

func compactHomeDialError(err error) string {
	if err == nil {
		return ""
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return "timeout"
	}
	if class := classifyHomeDialFailure(err); class != "" {
		return describeHomeDialFailure(class)
	}
	value := strings.TrimSpace(err.Error())
	if value == "" {
		return "unknown error"
	}
	return value
}

func classifyHomeDialFailure(err error) string {
	if err == nil {
		return ""
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ""
	}
	switch uintptr(errno) {
	case 10013: // WSAEACCES
		return homeDialFailureAccessDenied
	case 10024, 10055: // WSAEMFILE, WSAENOBUFS
		return homeDialFailureResourceLimit
	case 10049: // WSAEADDRNOTAVAIL
		return homeDialFailureAddressMissing
	default:
		return ""
	}
}

func describeHomeDialFailure(class string) string {
	switch class {
	case homeDialFailureAccessDenied:
		return "Windows 拒绝本地 TCP socket（WSAEACCES/10013），常见于临时端口耗尽、WFP/防火墙过滤或端口保留冲突"
	case homeDialFailureResourceLimit:
		return "Windows 本地 TCP socket/端口资源耗尽（WSAEMFILE/10024 或 WSAENOBUFS/10055）"
	case homeDialFailureAddressMissing:
		return "WireGuard 隧道 IPv4 源地址暂不可用于本地 TCP socket（WSAEADDRNOTAVAIL/10049）"
	default:
		return "Windows 本地 TCP socket 探测失败"
	}
}

func commonHomeDialFailureClass(attempts []homeInternetAttempt) string {
	if len(attempts) == 0 || attempts[0].OK || attempts[0].FailureClass == "" {
		return ""
	}
	class := attempts[0].FailureClass
	for _, attempt := range attempts[1:] {
		if attempt.OK || attempt.FailureClass != class {
			return ""
		}
	}
	return class
}

func homeCounterDelta(before, after uint64) uint64 {
	if after < before {
		return after
	}
	return after - before
}

func homeTrafficRunaway(sent, received uint64) bool {
	if sent < homeRunawaySentDelta {
		return false
	}
	if received == 0 {
		return true
	}
	return sent/received >= homeRunawayRatio
}

func evaluateHomeTrafficGuard(baseline, current homeWireGuardMetrics, stage string) (string, string) {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "启动探测"
	}
	if current.PeerCount != 1 {
		return homeDialFailureGuardUnavailable, fmt.Sprintf("%s无法启用单 peer 异常流量保护：当前 WireGuard 配置包含 %d 个 peer；家庭全隧道配置必须且只能包含一个服务端 peer", stage, current.PeerCount)
	}
	sentDelta := homeCounterDelta(baseline.SentBytes, current.SentBytes)
	receivedDelta := homeCounterDelta(baseline.ReceivedBytes, current.ReceivedBytes)
	if !homeTrafficRunaway(sentDelta, receivedDelta) {
		return "", ""
	}
	return homeDialFailureTrafficSurge, fmt.Sprintf("%s WireGuard 发送 +%d B、接收 +%d B，上行增量达到安全阈值且收发比异常；已触发安全中止。优先检查 IPv6 Endpoint 外层路由回环以及物理网卡 Forwarding/WeakHostSend；若启动时确有大规模正常上行，也可能触发这项偏安全的保护", stage, sentDelta, receivedDelta)
}

func probeHomeTrafficGuard(ctx context.Context, tunnelName string, baseline homeWireGuardMetrics, stage string) (string, string) {
	current, err := probeHomeWireGuardMetrics(ctx, tunnelName)
	if err != nil {
		return homeDialFailureGuardUnavailable, fmt.Sprintf("%s无法读取 WireGuard 公开流量计数，已安全中止：%v", strings.TrimSpace(stage), err)
	}
	return evaluateHomeTrafficGuard(baseline, current, stage)
}

func describeHomeProbeTraffic(sent, received uint64) string {
	switch {
	case sent == 0:
		return "探测流量没有进入 WireGuard，优先检查 Windows 路由、第三方防火墙以及 Clash TUN 是否关闭"
	case received == 0:
		return "WireGuard 发送计数增加但没有回包，不能证明数据已离开物理网卡；请结合 Endpoint 外层路由、物理网卡的 Forwarding/WeakHostSend、链路收发和服务端回程继续定位"
	default:
		return "WireGuard 双向字节均有增加，但 TCP 建连仍失败，优先检查防火墙、conntrack 或路径 MTU"
	}
}

func probePreferredHomeNetworkStatus(ctx context.Context, preferred string, profiles []string) homeNetworkSnapshot {
	status := probeHomeNetworkStatus(ctx, preferred)
	if status.Running {
		return status
	}
	for _, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(profile), strings.TrimSpace(preferred)) {
			continue
		}
		candidate := probeHomeNetworkStatus(ctx, profile)
		if candidate.Running {
			return candidate
		}
	}
	return status
}

func probeHomeNetworkStatus(ctx context.Context, tunnelName string) homeNetworkSnapshot {
	return probeHomeNetworkStatusInternal(ctx, tunnelName, true)
}

func probeHomeNetworkStatusInternal(ctx context.Context, tunnelName string, verifyProtection bool) homeNetworkSnapshot {
	result := homeNetworkSnapshot{TunnelName: strings.TrimSpace(tunnelName)}
	if _, err := resolveWireGuardExecutable(); err != nil {
		return result
	}
	result.Installed = true
	if result.TunnelName == "" || validateHomeTunnelName(result.TunnelName) != nil {
		return result
	}
	service, err := queryHomeTunnelService(ctx, result.TunnelName)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !service.Exists {
		result.ServiceState = "not-installed"
		return result
	}
	result.ServiceState = service.State
	result.Running = service.State == "running" || service.State == "start-pending"
	if !result.Running {
		return result
	}
	if verifyProtection {
		manager, managerErr := newHomeRoutingManager()
		if managerErr != nil {
			result.ProtectionError = "无法定位 BKNetwork Endpoint 路由恢复记录：" + managerErr.Error()
		} else {
			state, loadErr := manager.load()
			switch {
			case loadErr != nil:
				result.ProtectionError = loadErr.Error()
			case state == nil || !strings.EqualFold(state.TunnelName, result.TunnelName):
				result.ProtectionError = "未找到该运行隧道的 BKNetwork Endpoint 路由保护记录"
			default:
				result.Managed = true
				endpoints, endpointErr := probeHomeWireGuardEndpoints(ctx, result.TunnelName)
				if endpointErr != nil {
					result.ProtectionError = endpointErr.Error()
				} else if protectionErr := manager.verifyProtection(ctx, result.TunnelName, endpoints); protectionErr != nil {
					result.ProtectionError = protectionErr.Error()
				} else {
					result.ProtectionHealthy = true
				}
			}
		}
	}

	metrics, err := probeHomeWireGuardMetrics(ctx, result.TunnelName)
	if err != nil {
		result.Error = fmt.Sprintf("无法读取 WireGuard 握手状态：%v", err)
		return result
	}
	result.ReceivedBytes = metrics.ReceivedBytes
	result.SentBytes = metrics.SentBytes
	result.PeerCount = metrics.PeerCount
	if !metrics.HandshakeAt.IsZero() {
		result.LastHandshakeAt = metrics.HandshakeAt.Format(time.RFC3339)
		age := time.Since(metrics.HandshakeAt)
		if age < 0 {
			age = 0
		}
		result.HandshakeAgeSecs = int64(age.Seconds())
		result.Connected = age <= homeHandshakeFreshFor
	}
	return result
}

func triggerHomeTunnelTraffic(ctx context.Context) {
	// A single UDP payload causes WireGuard to create the first outbound packet
	// and therefore start the handshake immediately. It is not a WARP or
	// Cloudflare dependency; any IPv6 destination covered by AllowedIPs works.
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", "[2001:4860:4860::8888]:53")
	if err == nil {
		_, _ = conn.Write([]byte{0})
		_ = conn.Close()
	}
}

func waitForHomeNetworkConnected(ctx context.Context, tunnelName string, baseline homeWireGuardMetrics) homeNetworkSnapshot {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		status := probeHomeNetworkStatusInternal(ctx, tunnelName, false)
		if status.Error != "" {
			if status.Running {
				status.FailureClass = homeDialFailureGuardUnavailable
			}
			return status
		}
		if status.PeerCount != 1 && status.Error == "" {
			status.Connected = false
			status.FailureClass = homeDialFailureGuardUnavailable
			status.Error = fmt.Sprintf("无法启用单 peer 异常流量保护：当前 WireGuard 配置包含 %d 个 peer；家庭全隧道配置必须且只能包含一个服务端 peer", status.PeerCount)
			return status
		}
		sentDelta := homeCounterDelta(baseline.SentBytes, status.SentBytes)
		receivedDelta := homeCounterDelta(baseline.ReceivedBytes, status.ReceivedBytes)
		if homeTrafficRunaway(sentDelta, receivedDelta) {
			status.Connected = false
			status.FailureClass = homeDialFailureTrafficSurge
			status.Error = fmt.Sprintf("等待握手期间 WireGuard 发送 +%d B、接收 +%d B，上行增量达到安全阈值且收发比异常；已触发安全中止，优先检查 IPv6 Endpoint 外层路由回环以及物理网卡 Forwarding/WeakHostSend", sentDelta, receivedDelta)
			return status
		}
		if status.Connected {
			return status
		}
		select {
		case <-ctx.Done():
			return status
		case <-ticker.C:
		}
	}
}
