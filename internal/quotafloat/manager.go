// Package quotafloat keeps Quota Float's startup-only HTTP client in sync with
// BKNetwork's existing ChatGPT routing switch. Only the child process receives
// proxy environment variables; the user's environment and app files stay intact.
package quotafloat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Process struct {
	PID     uint32
	Started uint64
	Path    string
}

type processID struct {
	pid     uint32
	started uint64
}

func (p Process) id() processID { return processID{p.PID, p.Started} }

type Snapshot struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Manager struct {
	mu       sync.Mutex
	list     func() ([]Process, error)
	restart  func(Process, string) (Process, error)
	enabled  bool
	address  string
	managed  map[processID]string
	failed   map[processID]error
	snapshot Snapshot
}

func NewManager() *Manager {
	return newManager(listProcesses, restartProcess)
}

func newManager(list func() ([]Process, error), restart func(Process, string) (Process, error)) *Manager {
	return &Manager{
		list: list, restart: restart,
		managed: make(map[processID]string), failed: make(map[processID]error),
		snapshot: Snapshot{Status: "disabled", Detail: "quota-float 自动适配未开启"},
	}
}

// Configure also retries a previous failure when the user reapplies the switch.
// Background checks do not repeatedly restart a failing instance.
func (m *Manager) Configure(enabled bool, address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled, m.address = enabled, address
	m.failed = make(map[processID]error)
	return m.reconcileLocked()
}

// Restore makes one bounded retry for transient process-exit races. Persistent
// failures leave the original instance running and report a manual recovery.
func (m *Manager) Restore() error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = m.Configure(false, "")
		if err == nil {
			return nil
		}
	}
	return err
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

// Run handles Quota Float starting after BKNetwork, including Windows login.
// A successfully adapted instance is left running until routing changes.
func (m *Manager) Run(ctx context.Context, changed func(Snapshot)) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			previous := m.snapshot
			if m.enabled || len(m.managed) > 0 {
				_ = m.reconcileLocked()
			}
			next := m.snapshot
			m.mu.Unlock()
			if changed != nil && next != previous {
				changed(next)
			}
		}
	}
}

func (m *Manager) reconcileLocked() error {
	if !m.enabled && len(m.managed) == 0 {
		m.snapshot = Snapshot{Status: "disabled", Detail: "quota-float 已恢复用户原有代理环境"}
		return nil
	}
	processes, err := m.list()
	if err != nil {
		return m.setError(fmt.Errorf("检测 quota-float 失败：%w", err))
	}
	live := make(map[processID]bool, len(processes))
	for _, p := range processes {
		live[p.id()] = true
	}
	for id := range m.managed {
		if !live[id] {
			delete(m.managed, id)
		}
	}
	for id := range m.failed {
		if !live[id] {
			delete(m.failed, id)
		}
	}
	var failures []error
	for _, p := range processes {
		id := p.id()
		previousAddress, managed := m.managed[id]
		if (m.enabled && managed && previousAddress == m.address) || (!m.enabled && !managed) {
			continue
		}
		if previousErr, attempted := m.failed[id]; attempted {
			failures = append(failures, previousErr)
			continue
		}
		address := ""
		if m.enabled {
			address = m.address
		}
		next, restartErr := m.restart(p, address)
		if restartErr != nil {
			m.failed[id] = restartErr
			failures = append(failures, restartErr)
			continue
		}
		delete(m.managed, id)
		delete(m.failed, id)
		if m.enabled {
			m.managed[next.id()] = address
		}
	}
	if len(failures) > 0 {
		return m.setError(errors.Join(failures...))
	}
	switch {
	case !m.enabled:
		m.snapshot = Snapshot{Status: "disabled", Detail: "quota-float 已恢复用户原有代理环境"}
	case len(m.managed) > 0:
		m.snapshot = Snapshot{Status: "active", Detail: "quota-float 已使用 Clash 代理重启，正在自动刷新额度"}
	default:
		m.snapshot = Snapshot{Status: "waiting", Detail: "quota-float 启动后将自动适配 Clash"}
	}
	return nil
}

func (m *Manager) setError(err error) error {
	if !m.enabled {
		err = fmt.Errorf("%w；请完全退出并重开 quota-float 以恢复用户默认环境", err)
	}
	m.snapshot = Snapshot{Status: "error", Detail: "quota-float 适配未完成：" + err.Error()}
	return err
}

// Windows environment names are case-insensitive. Remove every spelling of a
// proxy key so an inherited lowercase value cannot override the selected Clash.
// On restore, use the original user's standard environment unchanged.
func proxyEnvironment(base []string, address string) []string {
	result := make([]string, 0, len(base)+4)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			if address != "" {
				continue
			}
		}
		result = append(result, entry)
	}
	if address != "" {
		proxyURL := "http://" + address
		result = append(result,
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"ALL_PROXY="+proxyURL,
			"NO_PROXY=localhost,127.0.0.1,::1",
		)
	}
	return result
}
