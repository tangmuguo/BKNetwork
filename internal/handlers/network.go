package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func psBool(b bool) string {
	if b {
		return "$true"
	}
	return "$false"
}

type networkComponentBinding struct {
	ComponentID string `json:"ComponentID"`
	Enabled     bool   `json:"Enabled"`
}

func parseNetworkComponentBindings(raw string) (ipv4, ipv6 bool, foundIPv4, foundIPv6 bool, err error) {
	bindings, err := decodeJSONList[networkComponentBinding](raw)
	if err != nil {
		return false, false, false, false, err
	}
	for _, binding := range bindings {
		switch strings.ToLower(strings.TrimSpace(binding.ComponentID)) {
		case "ms_tcpip":
			ipv4, foundIPv4 = binding.Enabled, true
		case "ms_tcpip6":
			ipv6, foundIPv6 = binding.Enabled, true
		}
	}
	return ipv4, ipv6, foundIPv4, foundIPv6, nil
}

func waitForNetworkModeApplied(ifName string, wantIPv4, wantIPv6 bool) error {
	escaped := escapePowerShellSingleQuotedString(ifName)
	ctx, cancel := context.WithTimeout(context.Background(), timeoutMedium)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var lastState string
	for {
		script := fmt.Sprintf("Get-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip,ms_tcpip6 -ErrorAction Stop | Select-Object ComponentID,Enabled | ConvertTo-Json -Compress", escaped)
		raw, execErr := execWithTimeout(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		if execErr == nil || strings.TrimSpace(raw) != "" {
			ipv4, ipv6, foundIPv4, foundIPv6, parseErr := parseNetworkComponentBindings(raw)
			if parseErr == nil {
				lastState = fmt.Sprintf("IPv4=%t, IPv6=%t", ipv4, ipv6)
				if foundIPv4 && foundIPv6 && ipv4 == wantIPv4 && ipv6 == wantIPv6 {
					return nil
				}
			} else {
				lastState = parseErr.Error()
			}
		} else {
			lastState = execErr.Error()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("网卡 %s 协议绑定未达到目标状态（期望 IPv4=%t, IPv6=%t；最后状态：%s）", ifName, wantIPv4, wantIPv6, lastState)
		case <-ticker.C:
		}
	}
}

func applyNetworkMode(ifName, mode string) (string, error) {
	var wantIPv4, wantIPv6 bool
	switch mode {
	case "ipv4":
		wantIPv4, wantIPv6 = true, false
	case "ipv6":
		wantIPv4, wantIPv6 = false, true
	case "both":
		wantIPv4, wantIPv6 = true, true
	default:
		return "", fmt.Errorf("unknown mode: %s", mode)
	}

	escaped := escapePowerShellSingleQuotedString(ifName)
	v4, v6 := psBool(wantIPv4), psBool(wantIPv6)

	var script strings.Builder
	script.WriteString("$ErrorActionPreference='Stop';")
	script.WriteString(fmt.Sprintf("$b=Get-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip,ms_tcpip6 -ErrorAction SilentlyContinue", escaped))
	script.WriteString(";$cur=@{};$b|ForEach-Object{$cur[$_.ComponentID]=$_.Enabled}")
	script.WriteString(fmt.Sprintf(";$r='was:'+($cur['ms_tcpip'])+','+($cur['ms_tcpip6'])"))
	script.WriteString(fmt.Sprintf(";if($cur['ms_tcpip'] -ne $true -and %s -eq $true){Enable-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip -Confirm:$false;$r+='|en4'}", v4, escaped))
	script.WriteString(fmt.Sprintf(";if($cur['ms_tcpip'] -eq $true -and %s -eq $false){Disable-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip -Confirm:$false;$r+='|dis4'}", v4, escaped))
	script.WriteString(fmt.Sprintf(";if($cur['ms_tcpip6'] -ne $true -and %s -eq $true){Enable-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip6 -Confirm:$false;$r+='|en6'}", v6, escaped))
	script.WriteString(fmt.Sprintf(";if($cur['ms_tcpip6'] -eq $true -and %s -eq $false){Disable-NetAdapterBinding -Name '%s' -ComponentID ms_tcpip6 -Confirm:$false;$r+='|dis6'}", v6, escaped))
	script.WriteString(";$r")

	ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
	defer cancel()
	out, err := execWithTimeout(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script.String())
	if err != nil {
		return out, err
	}
	if err := waitForNetworkModeApplied(ifName, wantIPv4, wantIPv6); err != nil {
		return out, err
	}
	return out, nil
}

func getIPv6AdminState(ifName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
	defer cancel()

	raw, err := execWithTimeout(ctx, "netsh", "interface", "ipv6", "show", "interface")
	if err != nil && raw == "" {
		return false, err
	}
	// Parse netsh output to find the adapter and its state
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 5 {
			continue
		}
		name := strings.Join(fields[4:], " ")
		if !strings.EqualFold(name, ifName) {
			continue
		}
		state := strings.ToLower(fields[3])
		return state == "connected", nil
	}
	return false, errors.New("adapter not found")
}
