package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"bknetwork/internal/events"
	appsettings "bknetwork/internal/settings"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return origin == "http://localhost:13335" || origin == "http://127.0.0.1:13335"
	},
}

const (
	timeoutDial          = 3 * time.Second
	timeoutShort         = 5 * time.Second
	timeoutMedium        = 8 * time.Second
	timeoutApply         = 12 * time.Second
	timeoutLong          = 15 * time.Second
	timeoutWarpConnect   = 24 * time.Second
	timeoutWarpProtocol  = 18 * time.Second
	timeoutWarpStop      = 15 * time.Second
	warpPrimaryAttempts  = 4
	warpFallbackAttempts = 1
)

var warpModeMu sync.Mutex

type apiResponse struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Detail  string      `json:"detail,omitempty"`
	Output  string      `json:"output,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

type settingsSnapshot struct {
	AutoStart        bool `json:"autoStart"`
	SilentStart      bool `json:"silentStart"`
	WarpAutoStart    bool `json:"warpAutoStart"`
	WarpAppAutoStart bool `json:"warpAppAutoStart"`
}

func writeJSON(w http.ResponseWriter, v interface{}, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func decodeJSONList[T any](raw string) ([]T, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []T{}, nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []T
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var one T
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		return nil, err
	}
	return []T{one}, nil
}

func execWithTimeout(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func notify(hub *events.Hub, typ, msg string, data interface{}) {
	if hub == nil {
		return
	}
	hub.Publish(events.Event{Type: typ, Message: msg, Data: data})
}

func requireAdmin(w http.ResponseWriter, hub *events.Hub, eventType string, payload interface{}) bool {
	ok, adminErr := isAdmin()
	if adminErr != nil {
		log.Printf("%s: isAdmin check failed: %v", eventType, adminErr)
	}
	if !ok {
		writeJSON(w, map[string]string{"error": "admin required"}, http.StatusForbidden)
		notify(hub, eventType+".error", "administrator privilege required", payload)
		return false
	}
	return true
}

func SwitchStackHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			IfName string `json:"ifName"`
			Mode   string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
			notify(hub, "switch.error", "invalid request body", nil)
			return
		}

		if !requireAdmin(w, hub, "switch", payload) {
			return
		}

		if payload.Mode != "ipv4" && payload.Mode != "ipv6" && payload.Mode != "both" {
			writeJSON(w, map[string]string{"error": "unknown mode"}, http.StatusBadRequest)
			notify(hub, "switch.error", "unknown mode", payload)
			return
		}

		out, err := applyNetworkMode(payload.IfName, payload.Mode)
		if err != nil {
			result := map[string]interface{}{
				"error":  "command failed",
				"detail": err.Error(),
				"output": out,
			}
			writeJSON(w, result, http.StatusInternalServerError)
			notify(hub, "switch.error", "failed to switch network stack", map[string]interface{}{
				"request": payload,
				"detail":  err.Error(),
				"output":  out,
			})
			return
		}

		result := map[string]interface{}{"ok": true, "output": out}
		writeJSON(w, result, http.StatusOK)
		notify(hub, "switch.ok", "network stack updated", map[string]interface{}{
			"request": payload,
			"output":  strings.TrimSpace(out),
		})
	}
}

func WarpHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			Action string `json:"action"`
			IfName string `json:"ifName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
			notify(hub, "warp.error", "invalid request body", nil)
			return
		}

		if !requireAdmin(w, hub, "warp", payload) {
			return
		}

		if _, err := exec.LookPath("warp-cli"); err != nil {
			writeJSON(w, map[string]string{"error": "warp-cli not found; please install Cloudflare WARP client"}, http.StatusBadRequest)
			notify(hub, "warp.error", "warp-cli not found", nil)
			return
		}

		if payload.Action == "start" && strings.TrimSpace(payload.IfName) != "" {
			preflightCtx, preflightCancel := context.WithTimeout(context.Background(), timeoutShort)
			preflight := probeWarpPreflight(preflightCtx, payload.IfName)
			preflightCancel()
			if !preflight.OK {
				writeJSON(w, map[string]interface{}{"error": "WARP 启动检查未通过", "code": preflight.Code, "detail": preflight.Message, "preflight": preflight}, http.StatusConflict)
				notify(hub, "warp.error", preflight.Message, preflight)
				return
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
		defer cancel()

		out, err := applyWarpAction(ctx, payload.Action)
		if err != nil && errors.Is(err, errUnknownWarpAction) {
			writeJSON(w, map[string]string{"error": "unknown action"}, http.StatusBadRequest)
			notify(hub, "warp.error", "unknown action", payload)
			return
		}

		if err != nil {
			result := map[string]interface{}{"error": "warp error", "detail": err.Error(), "output": out}
			writeJSON(w, result, http.StatusInternalServerError)
			notify(hub, "warp.error", "failed to update warp state", map[string]interface{}{
				"request": payload,
				"detail":  err.Error(),
				"output":  out,
			})
			return
		}

		warpProbe := probeWarpStatus(ctx)
		if payload.Action == "stop" {
			clearFreeFlowRuntimeState(payload.IfName)
		}

		result := map[string]interface{}{"ok": true, "output": out, "connected": warpProbe.Connected, "status": warpProbe.Status}
		writeJSON(w, result, http.StatusOK)
		notify(hub, "warp.ok", "warp state updated", map[string]interface{}{
			"request":   payload,
			"output":    strings.TrimSpace(out),
			"connected": warpProbe.Connected,
			"status":    warpProbe.Status,
		})
	}
}

func WarpModeHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			IfName  string `json:"ifName"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
			return
		}
		payload.IfName = strings.TrimSpace(payload.IfName)
		if payload.IfName == "" {
			writeJSON(w, map[string]string{"error": "missing ifName"}, http.StatusBadRequest)
			return
		}
		if !requireAdmin(w, hub, "warp-mode", payload) {
			return
		}
		if _, err := exec.LookPath("warp-cli"); err != nil {
			writeJSON(w, map[string]string{"error": "未找到 warp-cli，请先安装 Cloudflare One Client"}, http.StatusBadRequest)
			return
		}

		warpModeMu.Lock()
		defer warpModeMu.Unlock()

		if !payload.Enabled {
			warpOut, warpErr := disconnectWarpAndWait()
			stackOut, stackErr := applyNetworkMode(payload.IfName, "both")
			if stackErr != nil {
				writeJSON(w, map[string]interface{}{"error": "恢复双栈失败", "detail": stackErr.Error(), "warpOutput": warpOut, "stackOutput": stackOut}, http.StatusInternalServerError)
				return
			}
			if warpErr != nil {
				writeJSON(w, map[string]interface{}{"error": "WARP 断开失败，但已恢复双栈", "detail": warpErr.Error(), "warpOutput": warpOut}, http.StatusBadGateway)
				return
			}
			clearFreeFlowRuntimeState(payload.IfName)
			result := map[string]interface{}{"ok": true, "enabled": false, "rolledBack": false}
			writeJSON(w, result, http.StatusOK)
			notify(hub, "warp-mode.ok", "WARP free-flow mode disabled", result)
			return
		}

		preflightCtx, preflightCancel := context.WithTimeout(context.Background(), timeoutShort)
		preflight := probeWarpPreflight(preflightCtx, payload.IfName)
		preflightCancel()
		if !preflight.OK {
			result := map[string]interface{}{"error": "WARP 启动检查未通过", "code": preflight.Code, "detail": preflight.Message, "preflight": preflight, "rolledBack": false}
			writeJSON(w, result, http.StatusConflict)
			notify(hub, "warp-mode.error", preflight.Message, result)
			return
		}

		settingsCtx, settingsCancel := context.WithTimeout(context.Background(), timeoutShort)
		originalSettings := probeWarpSettings(settingsCtx)
		settingsCancel()
		originalProtocol, protocolParseErr := normalizeWarpTunnelProtocol(originalSettings.TunnelProtocol)
		if originalSettings.Error != "" || protocolParseErr != nil {
			detail := originalSettings.Error
			if detail == "" {
				detail = protocolParseErr.Error()
			}
			result := map[string]interface{}{
				"error": "无法读取 WARP 隧道协议，未修改网络", "detail": detail,
				"settings": originalSettings, "rolledBack": false,
			}
			writeJSON(w, result, http.StatusBadGateway)
			notify(hub, "warp-mode.error", result["error"].(string), result)
			return
		}

		// Stop any retry loop before changing protocol or the Windows IP stack.
		// This also makes protocol changes deterministic when the previous MASQUE
		// attempt is still being torn down by the Cloudflare daemon.
		disconnectOut, disconnectErr := disconnectWarpAndWait()
		if disconnectErr != nil {
			result := map[string]interface{}{
				"error": "无法停止当前 WARP 连接，未修改网络", "detail": disconnectErr.Error(),
				"warpOutput": strings.TrimSpace(disconnectOut), "rolledBack": false,
			}
			writeJSON(w, result, http.StatusBadGateway)
			notify(hub, "warp-mode.error", result["error"].(string), result)
			return
		}

		// The last two repeatable campus-network successes both used MASQUE over
		// an IPv6 underlay. Make that the deterministic first choice instead of
		// inheriting a WireGuard fallback left behind by an earlier failure.
		activeProtocol := "MASQUE"
		protocolChanged := !warpTunnelProtocolMatches(originalProtocol, activeProtocol)
		protocolOutput := ""
		if protocolChanged {
			protocolCtx, protocolCancel := context.WithTimeout(context.Background(), timeoutWarpProtocol)
			primaryProtocolOut, primaryProtocolErr := setWarpTunnelProtocolVerified(protocolCtx, activeProtocol)
			protocolCancel()
			protocolOutput = strings.TrimSpace(primaryProtocolOut)
			if primaryProtocolErr != nil {
				result := map[string]interface{}{
					"error": "无法将 WARP 切换到首选的 MASQUE 协议，未修改网络", "detail": primaryProtocolErr.Error(),
					"protocolOutput": protocolOutput, "originalProtocol": originalProtocol, "rolledBack": false,
				}
				writeJSON(w, result, http.StatusBadGateway)
				notify(hub, "warp-mode.error", result["error"].(string), result)
				return
			}
		}

		stackOut, stackErr := applyNetworkMode(payload.IfName, "ipv6")
		if stackErr != nil {
			_, _ = applyNetworkMode(payload.IfName, "both")
			protocolRestoreError := ""
			if protocolChanged {
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), timeoutWarpProtocol)
				_, restoreErr := setWarpTunnelProtocolVerified(restoreCtx, originalProtocol)
				restoreCancel()
				if restoreErr != nil {
					protocolRestoreError = restoreErr.Error()
				}
			}
			result := map[string]interface{}{"error": "切换到仅 IPv6 失败", "detail": stackErr.Error(), "stackOutput": stackOut, "rolledBack": true}
			if protocolRestoreError != "" {
				result["protocolRestoreError"] = protocolRestoreError
			}
			writeJSON(w, result, http.StatusInternalServerError)
			return
		}

		// Binding changes are asynchronous from the Cloudflare daemon's point of
		// view. Do not issue connect until its own network view has dropped IPv4;
		// otherwise Happy Eyeballs can still win on a stale IPv4 candidate.
		underlayCtx, underlayCancel := context.WithTimeout(context.Background(), timeoutApply)
		underlay := waitForWarpIPv6Underlay(underlayCtx, payload.IfName)
		underlayCancel()
		if !underlay.OK {
			rollbackOut, rollbackErr := applyNetworkMode(payload.IfName, "both")
			protocolRestored := !protocolChanged
			protocolRestoreError := ""
			if protocolChanged {
				restoreCtx, restoreCancel := context.WithTimeout(context.Background(), timeoutWarpProtocol)
				_, restoreErr := setWarpTunnelProtocolVerified(restoreCtx, originalProtocol)
				restoreCancel()
				protocolRestored = restoreErr == nil
				if restoreErr != nil {
					protocolRestoreError = restoreErr.Error()
				}
			}
			result := map[string]interface{}{
				"error": "Cloudflare 未刷新为仅 IPv6 外层网络，已恢复双栈", "detail": underlay.Message,
				"underlay": underlay, "stackOutput": stackOut, "rollbackOutput": strings.TrimSpace(rollbackOut),
				"rolledBack": rollbackErr == nil, "protocolRestored": protocolRestored,
			}
			if rollbackErr != nil {
				result["rollbackError"] = rollbackErr.Error()
			}
			if protocolRestoreError != "" {
				result["protocolRestoreError"] = protocolRestoreError
			}
			clearFreeFlowRuntimeState(payload.IfName)
			writeJSON(w, result, http.StatusBadGateway)
			notify(hub, "warp-mode.error", result["error"].(string), result)
			return
		}

		warpOut, warpProbe, warpErr, attempts := tryWarpConnectionRounds(warpPrimaryAttempts, timeoutWarpConnect, payload.IfName)
		fallbackAttempted := false
		if !warpProbe.Connected {
			fallbackAttempted = true
			fallbackProtocol := "WireGuard"
			protocolCtx, protocolCancel := context.WithTimeout(context.Background(), timeoutWarpProtocol)
			fallbackProtocolOut, fallbackProtocolErr := setWarpTunnelProtocolVerified(protocolCtx, fallbackProtocol)
			protocolCancel()
			protocolOutput = strings.TrimSpace(protocolOutput + "\n" + fallbackProtocolOut)
			if fallbackProtocolErr != nil {
				warpErr = fmt.Errorf("%v；MASQUE 重试均失败，且无法切换到 %s：%w", warpErr, fallbackProtocol, fallbackProtocolErr)
			} else {
				activeProtocol = fallbackProtocol
				protocolChanged = !warpTunnelProtocolMatches(originalProtocol, activeProtocol)
				fallbackOut, fallbackProbe, fallbackErr, fallbackAttempts := tryWarpConnectionRounds(warpFallbackAttempts, timeoutWarpConnect, payload.IfName)
				attempts += fallbackAttempts
				warpOut = strings.TrimSpace(warpOut + "\n" + fallbackOut)
				warpProbe = fallbackProbe
				warpErr = fallbackErr
			}
		}
		if warpProbe.Connected {
			setFreeFlowRuntimeState("warp", payload.IfName)
			result := map[string]interface{}{
				"ok": true, "enabled": true, "connected": true, "status": warpProbe.Status,
				"protocol": activeProtocol, "protocolChanged": protocolChanged,
				"fallbackAttempted": fallbackAttempted, "attempts": attempts,
				"stabilitySeconds": int(warpConnectedStableFor / time.Second),
				"preflight":        preflight, "underlay": warpProbe.Underlay,
			}
			writeJSON(w, result, http.StatusOK)
			notify(hub, "warp-mode.ok", "WARP free-flow mode enabled and stable", result)
			return
		}

		_, _ = disconnectWarpAndWait()
		rollbackOut, rollbackErr := applyNetworkMode(payload.IfName, "both")
		protocolRestored := !protocolChanged
		protocolRestoreError := ""
		if protocolChanged {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), timeoutWarpProtocol)
			_, restoreErr := setWarpTunnelProtocolVerified(restoreCtx, originalProtocol)
			restoreCancel()
			protocolRestored = restoreErr == nil
			if restoreErr != nil {
				protocolRestoreError = restoreErr.Error()
			}
		}
		result := map[string]interface{}{
			"error":  "WARP 连接未通过稳定性检查，已自动恢复双栈",
			"detail": warpErr.Error(), "warpOutput": strings.TrimSpace(warpOut),
			"protocolOutput": strings.TrimSpace(protocolOutput), "rollbackOutput": strings.TrimSpace(rollbackOut),
			"rolledBack": rollbackErr == nil, "preflight": preflight,
			"protocol": activeProtocol, "originalProtocol": originalProtocol,
			"protocolChanged": protocolChanged, "protocolRestored": protocolRestored,
			"fallbackAttempted": fallbackAttempted, "attempts": attempts,
			"stabilitySeconds": int(warpConnectedStableFor / time.Second),
		}
		if protocolRestoreError != "" {
			result["protocolRestoreError"] = protocolRestoreError
		}
		if rollbackErr != nil {
			result["rollbackError"] = rollbackErr.Error()
			result["error"] = "WARP 连接失败，且自动恢复双栈失败"
		} else {
			clearFreeFlowRuntimeState(payload.IfName)
		}
		writeJSON(w, result, http.StatusBadGateway)
		notify(hub, "warp-mode.error", result["error"].(string), result)
	}
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func tryWarpConnection(wait time.Duration, ifName string) (string, warpSnapshot, error) {
	connectCtx, connectCancel := context.WithTimeout(context.Background(), timeoutShort)
	out, err := applyWarpAction(connectCtx, "start")
	connectCancel()
	if err != nil {
		// Some warp-cli builds keep the command open while the daemon is still
		// performing Happy Eyeballs. A CLI timeout must not cancel a healthy
		// background attempt: the daemon status is authoritative.
		probeCtx, probeCancel := context.WithTimeout(context.Background(), timeoutShort)
		commandProbe := probeWarpStatus(probeCtx)
		probeCancel()
		if !warpStatusIsInProgress(commandProbe) {
			return out, commandProbe, err
		}
		warning := fmt.Sprintf("warp-cli connect 返回 %v，但后台状态为 %s，继续等待", err, fallbackText(commandProbe.Status, "未知"))
		out = strings.TrimSpace(out + "\n" + warning)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), wait)
	probe := waitForWarpConnected(waitCtx, ifName)
	waitCancel()
	return out, probe, nil
}

func disconnectWarpAndWait() (string, error) {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), timeoutShort)
	before := probeWarpStatus(probeCtx)
	probeCancel()
	if normalizeWarpStatus(before.Status) == "disconnected" {
		return "already disconnected", nil
	}

	disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), timeoutShort)
	out, disconnectErr := applyWarpAction(disconnectCtx, "stop")
	disconnectCancel()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), timeoutWarpStop)
	probe := waitForWarpDisconnected(waitCtx)
	waitCancel()
	if normalizeWarpStatus(probe.Status) != "disconnected" || probe.Error != "" {
		waitErr := warpConnectionError(probe)
		if disconnectErr != nil {
			return out, fmt.Errorf("断开命令失败：%v；%w", disconnectErr, waitErr)
		}
		return out, waitErr
	}
	// The observed daemon state is authoritative. Older client builds may
	// return a non-zero CLI result when disconnect is requested twice.
	return out, nil
}

func tryWarpConnectionRounds(rounds int, wait time.Duration, ifName string) (string, warpSnapshot, error, int) {
	if rounds < 1 {
		rounds = 1
	}
	var outputs []string
	var lastProbe warpSnapshot
	var lastErr error
	for attempt := 1; attempt <= rounds; attempt++ {
		out, probe, err := tryWarpConnection(wait, ifName)
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			outputs = append(outputs, fmt.Sprintf("attempt %d: %s", attempt, trimmed))
		}
		lastProbe = probe
		if err == nil && probe.Connected {
			return strings.Join(outputs, "\n"), probe, nil, attempt
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = warpConnectionError(probe)
		}

		disconnectOut, disconnectErr := disconnectWarpAndWait()
		if trimmed := strings.TrimSpace(disconnectOut); trimmed != "" {
			outputs = append(outputs, fmt.Sprintf("attempt %d disconnect: %s", attempt, trimmed))
		}
		if disconnectErr != nil {
			return strings.Join(outputs, "\n"), lastProbe, fmt.Errorf("第 %d 次连接失败，且清理旧隧道失败：%w", attempt, disconnectErr), attempt
		}
		if attempt < rounds {
			// Cloudflare can briefly retain the previous socket and network view
			// after disconnect. Confirm the IPv6-only underlay again and give the
			// daemon a short quiet period before restarting at the preferred 443
			// endpoint. This turns repeated manual clicks into one controlled retry.
			underlayCtx, underlayCancel := context.WithTimeout(context.Background(), timeoutApply)
			underlay := waitForWarpIPv6Underlay(underlayCtx, ifName)
			underlayCancel()
			if !underlay.OK {
				return strings.Join(outputs, "\n"), lastProbe, fmt.Errorf("第 %d 次连接后 IPv6 外层网络未恢复：%s", attempt, underlay.Message), attempt
			}
			time.Sleep(1200 * time.Millisecond)
		}
	}
	return strings.Join(outputs, "\n"), lastProbe, lastErr, rounds
}

func warpConnectionError(probe warpSnapshot) error {
	reason := fallbackText(probe.Error, fallbackText(probe.Reason, "未知"))
	return fmt.Errorf("WARP 未能连接（状态：%s，原因：%s）", fallbackText(probe.Status, "未知"), reason)
}

func WarpStatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
		defer cancel()
		probe := probeWarpStatus(ctx)
		writeJSON(w, map[string]interface{}{
			"connected": probe.Connected,
			"status":    probe.Status,
			"reason":    probe.Reason,
			"error":     probe.Error,
		}, http.StatusOK)
	}
}

func DnsHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		var payload struct {
			IfName      string    `json:"ifName"`
			IPv4Servers *[]string `json:"ipv4Servers"`
			IPv6Servers *[]string `json:"ipv6Servers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
			notify(hub, "dns.error", "invalid request body", nil)
			return
		}

		if !requireAdmin(w, hub, "dns", payload) {
			return
		}

		if strings.TrimSpace(payload.IfName) == "" {
			writeJSON(w, map[string]string{"error": "missing ifName"}, http.StatusBadRequest)
			notify(hub, "dns.error", "missing ifName", payload)
			return
		}
		if payload.IPv4Servers == nil && payload.IPv6Servers == nil {
			writeJSON(w, map[string]string{"error": "no dns changes provided"}, http.StatusBadRequest)
			notify(hub, "dns.error", "no dns changes provided", payload)
			return
		}

		out, err := applyDnsServers(payload.IfName, payload.IPv4Servers, payload.IPv6Servers)
		if err != nil {
			writeJSON(w, map[string]interface{}{"error": "command failed", "detail": err.Error(), "output": out}, http.StatusInternalServerError)
			notify(hub, "dns.error", "failed to update dns servers", map[string]interface{}{
				"request": payload,
				"detail":  err.Error(),
				"output":  out,
			})
			return
		}

		writeJSON(w, map[string]interface{}{"ok": true, "output": out}, http.StatusOK)
		notify(hub, "dns.ok", "dns servers updated", map[string]interface{}{
			"request": payload,
			"output":  strings.TrimSpace(out),
		})
	}
}

func SettingsHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, err := appsettings.Load()
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			writeJSON(w, settingsSnapshot{AutoStart: cfg.AutoStart, SilentStart: cfg.SilentStart, WarpAutoStart: cfg.WarpAutoStart, WarpAppAutoStart: cfg.WarpAppAutoStart}, http.StatusOK)
		case http.MethodPost:
			var payload settingsSnapshot
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
				return
			}
			prevCfg, err := appsettings.Load()
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			cfg := prevCfg
			cfg.AutoStart = payload.AutoStart
			cfg.SilentStart = payload.SilentStart
			cfg.WarpAutoStart = payload.WarpAutoStart
			cfg.WarpAppAutoStart = payload.WarpAppAutoStart
			if err := appsettings.Save(cfg); err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			if prevCfg.AutoStart != cfg.AutoStart {
				if err := appsettings.ApplyStartupShortcut(cfg.AutoStart); err != nil {
					writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
					return
				}
			}
			if prevCfg.WarpAppAutoStart != cfg.WarpAppAutoStart {
				if err := appsettings.ApplyWarpAppStartupShortcut(cfg.WarpAppAutoStart); err != nil {
					writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
					return
				}
			}
			notify(hub, "settings.ok", "settings updated", settingsSnapshot{AutoStart: cfg.AutoStart, SilentStart: cfg.SilentStart, WarpAutoStart: cfg.WarpAutoStart, WarpAppAutoStart: cfg.WarpAppAutoStart})
			writeJSON(w, map[string]any{"ok": true, "settings": settingsSnapshot{AutoStart: cfg.AutoStart, SilentStart: cfg.SilentStart, WarpAutoStart: cfg.WarpAutoStart, WarpAppAutoStart: cfg.WarpAppAutoStart}}, http.StatusOK)
		default:
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
		}
	}
}

func WSHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("ws upgrade error:", err)
			return
		}
		defer c.Close()

		sub := hub.Subscribe(8)
		defer hub.Unsubscribe(sub)

		if err := c.WriteJSON(events.Event{Type: "hello", Message: "connected to BKNetwork"}); err != nil {
			log.Printf("ws write hello: %v", err)
			return
		}
		snap, snapErr := collectNetworkSnapshot()
		if snapErr != nil {
			log.Printf("ws collect snapshot: %v", snapErr)
		}
		if err := c.WriteJSON(events.Event{Type: "network.status", Message: "network snapshot", Data: snap}); err != nil {
			log.Printf("ws write snapshot: %v", err)
			return
		}

		for {
			event, ok := <-sub
			if !ok {
				return
			}
			if err := c.WriteJSON(event); err != nil {
				log.Println("ws write error:", err)
				return
			}
		}
	}
}

func StatusHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		last, hasLast := hub.Snapshot()
		var lastEvent interface{}
		if hasLast {
			lastEvent = last
		}
		admin, adminErr := isAdmin()
		var adminErrMsg string
		if adminErr != nil {
			adminErrMsg = adminErr.Error()
		}
		network, _ := collectNetworkSnapshot()
		writeJSON(w, map[string]interface{}{
			"service": map[string]interface{}{
				"name":    "BKNetwork",
				"version": "1.0.0",
			},
			"admin":      admin,
			"adminError": adminErrMsg,
			"connection": map[string]interface{}{
				"websocket": "/ws",
			},
			"lastEvent": lastEvent,
			"network":   network,
			"time":      time.Now().Format(time.RFC3339),
		}, http.StatusOK)
	}
}

func LatestVersionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeoutMedium)
		defer cancel()

		tag, err := fetchLatestReleaseTag(ctx)
		if err != nil {
			log.Printf("latest version check failed: %v", err)
			writeJSON(w, map[string]interface{}{"ok": true, "tag": ""}, http.StatusOK)
			return
		}

		writeJSON(w, map[string]interface{}{"ok": true, "tag": tag}, http.StatusOK)
	}
}
