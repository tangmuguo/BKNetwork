//go:build !windows

package settings

import "errors"

type SystemProxyPACState struct {
	URL     string
	Present bool
}

func ReadSystemProxyPAC() (SystemProxyPACState, error) {
	return SystemProxyPACState{}, errors.New("Windows system proxy is only supported on Windows")
}

func WriteSystemProxyPAC(SystemProxyPACState) error {
	return errors.New("Windows system proxy is only supported on Windows")
}
