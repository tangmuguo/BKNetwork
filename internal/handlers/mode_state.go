package handlers

import (
	"strings"
	"sync"
)

type freeFlowRuntimeState struct {
	Mode      string
	Interface string
}

var freeFlowRuntime = struct {
	sync.RWMutex
	state freeFlowRuntimeState
}{}

func setFreeFlowRuntimeState(mode, ifName string) {
	freeFlowRuntime.Lock()
	freeFlowRuntime.state = freeFlowRuntimeState{
		Mode:      strings.ToLower(strings.TrimSpace(mode)),
		Interface: strings.TrimSpace(ifName),
	}
	freeFlowRuntime.Unlock()
}

func clearFreeFlowRuntimeState(ifName string) {
	freeFlowRuntime.Lock()
	if strings.TrimSpace(ifName) == "" || strings.EqualFold(freeFlowRuntime.state.Interface, strings.TrimSpace(ifName)) {
		freeFlowRuntime.state = freeFlowRuntimeState{}
	}
	freeFlowRuntime.Unlock()
}

func getFreeFlowRuntimeState() freeFlowRuntimeState {
	freeFlowRuntime.RLock()
	state := freeFlowRuntime.state
	freeFlowRuntime.RUnlock()
	return state
}
