package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

const snapshotAdaptersCommand = `
$ErrorActionPreference='Stop'
[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false)
$adapters=@(Get-NetAdapter | Select-Object Name,Status,MacAddress,InterfaceDescription)
$bindings=@(Get-NetAdapterBinding -ComponentID ms_tcpip,ms_tcpip6 | Select-Object Name,ComponentID,Enabled)
@{Adapters=$adapters;Bindings=$bindings} | ConvertTo-Json -Depth 4 -Compress
`

const snapshotAdapterBasicsCommand = "Get-NetAdapter | Select-Object Name, Status, MacAddress, InterfaceDescription | ConvertTo-Json -Compress"

func parseSnapshotAdapterData(raw string) (string, map[string]bool, map[string]bool, error) {
	var data struct {
		Adapters json.RawMessage
		Bindings []struct {
			Name        string
			ComponentID string
			Enabled     bool
		}
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return "", nil, nil, err
	}
	if len(data.Adapters) == 0 || data.Bindings == nil {
		return "", nil, nil, fmt.Errorf("incomplete adapter snapshot")
	}
	if _, err := decodeJSONList[adapterBasic](string(data.Adapters)); err != nil {
		return "", nil, nil, err
	}
	ipv4 := make(map[string]bool)
	ipv6 := make(map[string]bool)
	for _, binding := range data.Bindings {
		name := strings.TrimSpace(binding.Name)
		if name == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(binding.ComponentID)) {
		case "ms_tcpip":
			ipv4[name] = binding.Enabled
		case "ms_tcpip6":
			ipv6[name] = binding.Enabled
		}
	}
	return string(data.Adapters), ipv4, ipv6, nil
}

// Use one PowerShell process and one binding enumeration for all three pieces
// of state. If batching fails, retain the previous independent-query fallback
// so an unavailable component cannot hide otherwise usable adapter data.
func collectSnapshotAdapterData(run func(context.Context, string, ...string) (string, error)) (basicRaw string, ipv4, ipv6 map[string]bool) {
	query := func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeoutShort)
		defer cancel()
		return run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	}
	raw, err := query(snapshotAdaptersCommand)
	if err == nil {
		basicRaw, ipv4, ipv6, err = parseSnapshotAdapterData(raw)
		if err == nil {
			return
		}
	}
	log.Printf("snapshot: combined adapter query failed, using independent queries: %v", err)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		var queryErr error
		basicRaw, queryErr = query(snapshotAdapterBasicsCommand)
		if queryErr != nil {
			log.Printf("snapshot: Get-NetAdapter failed: %v", queryErr)
		}
	}()
	bindingQuery := func(componentID string) map[string]bool {
		out, queryErr := query(adapterBindingCommand(componentID))
		if queryErr != nil && strings.TrimSpace(out) == "" {
			log.Printf("snapshot: %s binding failed: %v", componentID, queryErr)
			return nil
		}
		result, parseErr := parsePowerShellAdapterBindings(out)
		if parseErr != nil {
			log.Printf("snapshot: %s binding failed: %v", componentID, parseErr)
		}
		return result
	}
	go func() { defer wg.Done(); ipv4 = bindingQuery("ms_tcpip") }()
	go func() { defer wg.Done(); ipv6 = bindingQuery("ms_tcpip6") }()
	wg.Wait()
	return
}
