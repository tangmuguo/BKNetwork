//go:build windows

package settings

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	internetOptionRefresh         = 37
	internetOptionSettingsChanged = 39
)

var (
	wininetDLL         = windows.NewLazySystemDLL("wininet.dll")
	internetSetOptionW = wininetDLL.NewProc("InternetSetOptionW")
)

type SystemProxyPACState struct {
	URL     string
	Present bool
}

func ReadSystemProxyPAC() (SystemProxyPACState, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return SystemProxyPACState{}, fmt.Errorf("open Windows system proxy settings: %w", err)
	}
	defer key.Close()

	value, _, err := key.GetStringValue("AutoConfigURL")
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return SystemProxyPACState{}, nil
		}
		return SystemProxyPACState{}, fmt.Errorf("read Windows proxy auto-config URL: %w", err)
	}
	return SystemProxyPACState{URL: value, Present: true}, nil
}

func WriteSystemProxyPAC(state SystemProxyPACState) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsRegistryPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Windows system proxy settings: %w", err)
	}
	defer key.Close()

	if state.Present {
		if err := key.SetStringValue("AutoConfigURL", state.URL); err != nil {
			return fmt.Errorf("set Windows proxy auto-config URL: %w", err)
		}
	} else if err := key.DeleteValue("AutoConfigURL"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("clear Windows proxy auto-config URL: %w", err)
	}

	if err := notifySystemProxyChanged(); err != nil {
		return err
	}
	return nil
}

func notifySystemProxyChanged() error {
	for _, option := range []uintptr{internetOptionSettingsChanged, internetOptionRefresh} {
		result, _, callErr := internetSetOptionW.Call(0, option, 0, 0)
		if result == 0 {
			if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
				callErr = errors.New("InternetSetOptionW returned false")
			}
			return fmt.Errorf("notify Windows that system proxy settings changed: %w", callErr)
		}
	}
	return nil
}
