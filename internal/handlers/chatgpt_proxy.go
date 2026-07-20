package handlers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bknetwork/internal/events"
	appsettings "bknetwork/internal/settings"
)

const (
	chatGPTProxyPACURL       = "http://127.0.0.1:13335/api/v1/chatgpt-proxy.pac?v=7"
	defaultClashProxyAddress = "127.0.0.1:7897"
)

// This list follows OpenAI's public network allowlist for ChatGPT web and
// desktop apps. Entries are suffix-matched so that documented wildcard hosts
// such as *.chatgpt.com and *.oaiusercontent.com keep working.
var chatGPTProxyDomains = []string{
	"openai.com",
	"chatgpt.com",
	"ct.sendgrid.net",
	"intercom.io",
	"intercomcdn.com",
	"oaistatic.com",
	"oaiusercontent.com",
	"oaistatsig.com",
	"cdn.openaimerge.com",
	"cdn.workos.com",
	"challenges.cloudflare.com",
	"forwarder.workos.com",
	"humb.apple.com",
	"images.workoscdn.com",
	"js.stripe.com",
	"o207216.ingest.sentry.io",
	"o33249.ingest.sentry.io",
	"rum.browser-intake-datadoghq.com",
	"setup.workos.com",
	"workos.imgix.net",
}

type chatGPTProxySnapshot struct {
	Enabled      bool   `json:"enabled"`
	Active       bool   `json:"active"`
	ProxyAddress string `json:"proxyAddress"`
	PACURL       string `json:"pacURL"`
	ProxyOnline  bool   `json:"proxyOnline"`
	Detail       string `json:"detail,omitempty"`
}

func ChatGPTProxyPACHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
			return
		}

		cfg, err := appsettings.Load()
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
			return
		}
		address, err := normalizeClashProxyAddress(cfg.ClashProxyAddress)
		if err != nil || !cfg.ChatGPTClashEnabled {
			address = ""
		}

		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(buildChatGPTProxyPAC(address)))
	}
}

func ChatGPTProxyHandler(hub *events.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			snapshot, err := collectChatGPTProxySnapshot()
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			writeJSON(w, snapshot, http.StatusOK)
		case http.MethodPost:
			var payload struct {
				Enabled      bool   `json:"enabled"`
				ProxyAddress string `json:"proxyAddress"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				writeJSON(w, map[string]string{"error": "invalid json"}, http.StatusBadRequest)
				return
			}

			address, err := normalizeClashProxyAddress(payload.ProxyAddress)
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
				return
			}
			if payload.Enabled {
				if err := checkLocalProxy(address, 900*time.Millisecond); err != nil {
					writeJSON(w, map[string]string{
						"error":  "Clash 本地代理端口不可用",
						"detail": fmt.Sprintf("无法连接 %s，请先启动 Clash Verge，并确认填写的是 HTTP/mixed 端口：%v", address, err),
					}, http.StatusConflict)
					return
				}
			}

			if err := configureChatGPTProxy(payload.Enabled, address); err != nil {
				writeJSON(w, map[string]string{"error": "更新 ChatGPT 分流失败", "detail": err.Error()}, http.StatusInternalServerError)
				notify(hub, "chatgpt-proxy.error", err.Error(), payload)
				return
			}
			snapshot, err := collectChatGPTProxySnapshot()
			if err != nil {
				writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true, "state": snapshot}, http.StatusOK)
			notify(hub, "chatgpt-proxy.ok", "ChatGPT Clash routing updated", snapshot)
		default:
			writeJSON(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
		}
	}
}

func normalizeClashProxyAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultClashProxyAddress
	}
	if !strings.Contains(raw, ":") {
		raw = net.JoinHostPort("127.0.0.1", raw)
	}
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("Clash 代理地址格式错误，请使用 127.0.0.1:端口")
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("为避免泄露代理流量，只允许使用本机 Clash 地址 127.0.0.1:端口")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("Clash 代理端口必须是 1-65535")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}

func buildChatGPTProxyPAC(proxyAddress string) string {
	if proxyAddress == "" {
		return "function FindProxyForURL(url, host) { return \"DIRECT\"; }\n"
	}
	domains, _ := json.Marshal(chatGPTProxyDomains)
	return fmt.Sprintf(`// BKNetwork v7 - ChatGPT via Clash Verge, everything else direct/WARP.
var BKNETWORK_CHATGPT_DOMAINS = %s;
function FindProxyForURL(url, host) {
  host = String(host || "").toLowerCase();
  if (isPlainHostName(host) || host === "localhost" || host === "127.0.0.1" || host === "::1") {
    return "DIRECT";
  }
  for (var i = 0; i < BKNETWORK_CHATGPT_DOMAINS.length; i++) {
    var domain = BKNETWORK_CHATGPT_DOMAINS[i];
    if (host === domain || dnsDomainIs(host, "." + domain)) {
      return "PROXY %s";
    }
  }
  return "DIRECT";
}
`, domains, proxyAddress)
}

func checkLocalProxy(address string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

func isBKNetworkPAC(state appsettings.SystemProxyPACState) bool {
	return state.Present && strings.HasPrefix(strings.TrimSpace(state.URL), "http://127.0.0.1:13335/api/v1/chatgpt-proxy.pac")
}

func configureChatGPTProxy(enabled bool, address string) error {
	cfg, err := appsettings.Load()
	if err != nil {
		return err
	}
	originalCfg := cfg
	currentPAC, err := appsettings.ReadSystemProxyPAC()
	if err != nil {
		return err
	}

	if enabled {
		cfg.ChatGPTClashEnabled = true
		cfg.ClashProxyAddress = address
		if !isBKNetworkPAC(currentPAC) {
			cfg.ChatGPTClashPreviousPACURL = currentPAC.URL
			cfg.ChatGPTClashPreviousPACURLPresent = currentPAC.Present
		}
		if err := appsettings.WriteSystemProxyPAC(appsettings.SystemProxyPACState{URL: chatGPTProxyPACURL, Present: true}); err != nil {
			_ = appsettings.WriteSystemProxyPAC(currentPAC)
			return err
		}
	} else {
		if isBKNetworkPAC(currentPAC) {
			previous := appsettings.SystemProxyPACState{
				URL:     cfg.ChatGPTClashPreviousPACURL,
				Present: cfg.ChatGPTClashPreviousPACURLPresent,
			}
			if err := appsettings.WriteSystemProxyPAC(previous); err != nil {
				_ = appsettings.WriteSystemProxyPAC(currentPAC)
				return err
			}
		}
		cfg.ChatGPTClashEnabled = false
		cfg.ClashProxyAddress = address
		cfg.ChatGPTClashPreviousPACURL = ""
		cfg.ChatGPTClashPreviousPACURLPresent = false
	}

	if err := appsettings.Save(cfg); err != nil {
		_ = appsettings.WriteSystemProxyPAC(currentPAC)
		_ = appsettings.Save(originalCfg)
		return err
	}
	return nil
}

func collectChatGPTProxySnapshot() (chatGPTProxySnapshot, error) {
	cfg, err := appsettings.Load()
	if err != nil {
		return chatGPTProxySnapshot{}, err
	}
	address, addressErr := normalizeClashProxyAddress(cfg.ClashProxyAddress)
	if addressErr != nil {
		address = defaultClashProxyAddress
	}
	currentPAC, err := appsettings.ReadSystemProxyPAC()
	if err != nil {
		return chatGPTProxySnapshot{}, err
	}
	snapshot := chatGPTProxySnapshot{
		Enabled:      cfg.ChatGPTClashEnabled,
		Active:       cfg.ChatGPTClashEnabled && isBKNetworkPAC(currentPAC),
		ProxyAddress: address,
		PACURL:       chatGPTProxyPACURL,
	}
	if checkErr := checkLocalProxy(address, 250*time.Millisecond); checkErr == nil {
		snapshot.ProxyOnline = true
	} else if cfg.ChatGPTClashEnabled {
		snapshot.Detail = fmt.Sprintf("Clash 本地端口当前不可用：%v", checkErr)
	}
	if cfg.ChatGPTClashEnabled && !snapshot.Active {
		if snapshot.Detail != "" {
			snapshot.Detail += "；"
		}
		snapshot.Detail += "Windows 系统代理中的 BKNetwork PAC 未生效"
	}
	return snapshot, nil
}

// ActivateConfiguredChatGPTProxy reapplies the PAC after BKNetwork starts. It
// deliberately does not require Clash to be ready yet because the two apps may
// start concurrently during Windows login.
func ActivateConfiguredChatGPTProxy() error {
	cfg, err := appsettings.Load()
	if err != nil || !cfg.ChatGPTClashEnabled {
		return err
	}
	address, err := normalizeClashProxyAddress(cfg.ClashProxyAddress)
	if err != nil {
		return err
	}
	return configureChatGPTProxy(true, address)
}

// SuspendConfiguredChatGPTProxy restores the user's prior PAC on a graceful
// exit while keeping the feature enabled in BKNetwork for the next launch.
func SuspendConfiguredChatGPTProxy() error {
	cfg, err := appsettings.Load()
	if err != nil || !cfg.ChatGPTClashEnabled {
		return err
	}
	currentPAC, err := appsettings.ReadSystemProxyPAC()
	if err != nil || !isBKNetworkPAC(currentPAC) {
		return err
	}
	return appsettings.WriteSystemProxyPAC(appsettings.SystemProxyPACState{
		URL:     cfg.ChatGPTClashPreviousPACURL,
		Present: cfg.ChatGPTClashPreviousPACURLPresent,
	})
}
