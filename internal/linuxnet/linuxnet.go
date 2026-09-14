// Package linuxnet contains the Linux-only network state machine used by the
// local BKNetwork service.  It deliberately keeps all privileged operations
// behind a small command runner and never exposes WireGuard key material.
package linuxnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultWireGuardDir = "/etc/wireguard"
	stateFileName       = "state.json"
	commandTimeout      = 8 * time.Second
	connectTimeout      = 35 * time.Second
	handshakeFreshFor   = 3 * time.Minute
)

// Dependency describes a command or service required by one or more modes.
type Dependency struct {
	Name        string   `json:"name"`
	Available   bool     `json:"available"`
	RequiredFor []string `json:"requiredFor,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

// Interface is a conservative snapshot of a local Linux network interface.
// IPv4 and IPv6 values include their CIDR prefix where the operating system
// provides one.
type Interface struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	State    string   `json:"state"`
	IPv4     []string `json:"ipv4"`
	IPv6     []string `json:"ipv6"`
	Physical bool     `json:"physical"`
}

// WarpStatus contains status information returned by warp-cli.  Raw command
// output is intentionally excluded because clients may print local details.
type WarpStatus struct {
	Installed bool   `json:"installed"`
	Connected bool   `json:"connected"`
	Status    string `json:"status,omitempty"`
}

// WireGuardStatus contains only non-secret runtime metadata.  The handshake
// timestamp is RFC3339 and is omitted when the peer has never handshaken.
type WireGuardStatus struct {
	Installed       bool   `json:"installed"`
	Connected       bool   `json:"connected"`
	LatestHandshake string `json:"latestHandshake,omitempty"`
}

// Status is the JSON-safe public snapshot consumed by the Linux web/API
// layer.  Verified is true only after runtime transport, routes, handshake
// (where applicable), and IPv6 connectivity checks have succeeded.
type Status struct {
	Platform             string          `json:"platform"`
	Privileged           bool            `json:"privileged"`
	Dependencies         []Dependency    `json:"dependencies"`
	Interfaces           []Interface     `json:"interfaces"`
	RecommendedInterface string          `json:"recommendedInterface,omitempty"`
	Profiles             []string        `json:"profiles"`
	ProfilesError        string          `json:"profilesError,omitempty"`
	Mode                 string          `json:"mode"`
	Phase                string          `json:"phase"`
	Interface            string          `json:"interface,omitempty"`
	Profile              string          `json:"profile,omitempty"`
	IPv6Only             bool            `json:"ipv6Only"`
	Verified             bool            `json:"verified"`
	Message              string          `json:"message,omitempty"`
	Warp                 WarpStatus      `json:"warp"`
	WireGuard            WireGuardStatus `json:"wireguard"`
	RecoveryPending      bool            `json:"recoveryPending"`
	ClashAppProxy        bool            `json:"clashAppProxy"`
}

// Options provides safe seams for unit tests and distribution-specific paths.
// Production callers normally use NewManager, which selects the Ubuntu
// defaults.  A custom Runner is never required to implement path lookup; when
// it does not, exec.LookPath is used.
type Options struct {
	Runner            Runner
	WireGuardDir      string
	InterfaceProvider func(context.Context) ([]Interface, error)
	PathFinder        interface{ LookPath(string) (string, error) }
	NMCLI             string
	IP                string
	WarpCLI           string
	WG                string
	WGQuick           string
	Curl              string
	Resolvectl        string
	Resolvconf        string
	Privileged        func() bool
}

type commandNames struct {
	nmcli      string
	ip         string
	warpCLI    string
	wg         string
	wgQuick    string
	curl       string
	resolvectl string
	resolvconf string
}

// Manager owns one Linux networking lifecycle. A manager is safe for one HTTP
// server to call concurrently; Connect and Disconnect are serialized while
// Status remains a bounded read-only probe during an in-flight transition.
type Manager struct {
	opMu                 sync.Mutex
	active               atomic.Bool
	stateDir             string
	wireGuardDir         string
	runner               Runner
	pathFinder           pathFinder
	commands             commandNames
	interfaceProvider    func(context.Context) ([]Interface, error)
	privileged           func() bool
	desktopUserProvider  func(context.Context) ([]desktopUser, error)
	desktopAccountLookup func(string) (*user.User, error)
}

type persistentState struct {
	Version         int                    `json:"version"`
	Mode            string                 `json:"mode"`
	Interface       string                 `json:"interface"`
	Profile         string                 `json:"profile,omitempty"`
	Phase           string                 `json:"phase"`
	RecoveryPending bool                   `json:"recoveryPending"`
	Message         string                 `json:"message,omitempty"`
	UpdatedAt       time.Time              `json:"updatedAt"`
	Underlay        underlaySnapshot       `json:"underlay,omitempty"`
	DesktopProxies  []desktopProxySnapshot `json:"desktopProxies,omitempty"`
}

type underlaySnapshot struct {
	Interface      string `json:"interface,omitempty"`
	Connection     string `json:"connection,omitempty"`
	ConnectionUUID string `json:"connectionUuid,omitempty"`
	IPv4Method     string `json:"ipv4Method,omitempty"`
	IPv4Disabled   bool   `json:"ipv4Disabled"`
}

// NewManager returns a Linux manager using /etc/wireguard and the user's
// state directory.  Passing an explicit stateDir is recommended for a system
// service (for example /var/lib/bknetwork).
func NewManager(stateDir string) *Manager {
	return NewManagerWithOptions(stateDir, Options{})
}

// NewManagerWithRunner is convenient for package consumers that need to
// exercise the lifecycle with a fake command runner.  It does not grant any
// additional privileges.
func NewManagerWithRunner(stateDir string, runner Runner) *Manager {
	return NewManagerWithOptions(stateDir, Options{Runner: runner})
}

// NewManagerWithOptions constructs a manager with explicit command and
// filesystem seams.  It is intentionally exported so downstream Linux tests
// do not need to replace the host's /etc/wireguard directory.
func NewManagerWithOptions(stateDir string, options Options) *Manager {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = defaultStateDir()
	}
	if strings.TrimSpace(options.WireGuardDir) == "" {
		options.WireGuardDir = defaultWireGuardDir
	}
	if options.Runner == nil {
		options.Runner = commandRunner{}
	}
	pf, ok := options.PathFinder.(pathFinder)
	if !ok || pf == nil {
		pf = osPathFinder{}
	}
	return &Manager{
		stateDir:          filepath.Clean(stateDir),
		wireGuardDir:      filepath.Clean(options.WireGuardDir),
		runner:            options.Runner,
		pathFinder:        pf,
		interfaceProvider: options.InterfaceProvider,
		privileged:        options.Privileged,
		commands: commandNames{
			nmcli:      commandOr(options.NMCLI, "nmcli"),
			ip:         commandOr(options.IP, "ip"),
			warpCLI:    commandOr(options.WarpCLI, "warp-cli"),
			wg:         commandOr(options.WG, "wg"),
			wgQuick:    commandOr(options.WGQuick, "wg-quick"),
			curl:       commandOr(options.Curl, "curl"),
			resolvectl: commandOr(options.Resolvectl, "resolvectl"),
			resolvconf: commandOr(options.Resolvconf, "/usr/sbin/resolvconf"),
		},
	}
}

func commandOr(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func defaultStateDir() string {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" && filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "bknetwork")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "state", "bknetwork")
	}
	return filepath.Join(os.TempDir(), "bknetwork-state")
}

func (m *Manager) commandAvailable(name string) bool {
	_, err := m.pathFinder.LookPath(name)
	return err == nil
}

func (m *Manager) dependency(name string, requiredFor ...string) Dependency {
	available := m.commandAvailable(name)
	detail := ""
	if !available {
		detail = fmt.Sprintf("未找到 %s，请先安装对应软件包", name)
	} else {
		detail = "已找到可执行文件"
	}
	return Dependency{Name: name, Available: available, RequiredFor: requiredFor, Detail: detail}
}

func (m *Manager) dependencies() []Dependency {
	return []Dependency{
		m.dependency(m.commands.nmcli, "status", "direct", "warp", "wireguard"),
		m.dependency(m.commands.ip, "status", "direct", "warp", "wireguard"),
		m.dependency(m.commands.curl, "verification", "direct", "warp", "wireguard"),
		m.dependency(m.commands.warpCLI, "warp"),
		m.dependency(m.commands.wg, "wireguard"),
		m.dependency(m.commands.wgQuick, "wireguard"),
		m.dependency(m.commands.resolvectl, "dns verification"),
		m.dependency(m.commands.resolvconf, "wireguard DNS profiles"),
	}
}

func dependencyAvailable(deps []Dependency, command string) bool {
	for _, dependency := range deps {
		if dependency.Name == command {
			return dependency.Available
		}
	}
	return false
}

// Status collects read-only operating-system and tunnel information.  Any
// failed probe is reflected in Message/ProfilesError and never guessed as a
// successful connection.
func (m *Manager) Status(ctx context.Context) Status {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return m.statusLocked(ctx)
}

func (m *Manager) statusLocked(ctx context.Context) Status {
	if ctx == nil {
		ctx = context.Background()
	}
	result := Status{
		Platform:     "linux",
		Privileged:   m.isPrivileged(),
		Dependencies: m.dependencies(),
		Interfaces:   []Interface{},
		Profiles:     []string{},
		Mode:         "direct",
		Phase:        "idle",
		Warp:         WarpStatus{},
		WireGuard:    WireGuardStatus{},
	}

	interfaces, interfaceErr := m.collectInterfaces(ctx)
	result.Interfaces = interfaces
	if interfaceErr != nil {
		result.Message = joinMessages(result.Message, fmt.Sprintf("读取本机网络接口失败：%v", interfaceErr))
	} else {
		result.RecommendedInterface = recommendedInterface(interfaces)
	}

	profiles, profilesErr := m.listProfiles()
	result.Profiles = profiles
	if profilesErr != nil {
		result.ProfilesError = profilesErr.Error()
	}

	state, stateErr := m.readState()
	if stateErr != nil {
		result.RecoveryPending = true
		result.Phase = "recovery"
		result.Message = joinMessages(result.Message, fmt.Sprintf("无法读取持久化网络状态，请先检查 %s：%v", m.statePath(), stateErr))
	}
	if state != nil {
		liveOperation := m.active.Load()
		result.Mode = fallbackText(state.Mode, "direct")
		result.Phase = fallbackText(state.Phase, "idle")
		result.Interface = state.Interface
		result.Profile = state.Profile
		result.ClashAppProxy = state.Mode == "warp" && len(state.DesktopProxies) > 0
		result.RecoveryPending = state.RecoveryPending || state.Phase == "connecting" || state.Phase == "disconnecting" || state.Phase == "recovery"
		if liveOperation && (state.Phase == "connecting" || state.Phase == "disconnecting") {
			result.RecoveryPending = false
		}
		if state.Message != "" {
			result.Message = joinMessages(result.Message, state.Message)
		}
	}

	result.Warp = m.probeWarp(ctx)
	tunnelInterface := result.Interface
	if state != nil && state.Profile != "" {
		tunnelInterface = normalizeProfileName(state.Profile)
	}
	result.WireGuard = m.probeWireGuard(ctx, tunnelInterface)
	physicalReady := false
	if selected, found := findInterface(result.Interfaces, result.Interface); found {
		physicalReady = selected.Physical && interfaceStateUsable(selected.State) && hasGlobalIPv6(selected.IPv6) && len(usableIPv4(selected.IPv4)) == 0 && noOtherPhysicalEgress(result.Interfaces, result.Interface)
	}

	if result.Mode == "warp" && result.Warp.Connected {
		result.IPv6Only = physicalReady
		if state != nil && state.Phase == "connected" && !result.RecoveryPending {
			verified, message := m.verifyRuntime(ctx, "warp", result.Interface, "")
			result.Verified = verified
			if !verified {
				result.Phase = "error"
				result.RecoveryPending = true
				result.Message = joinMessages(result.Message, message)
			}
		}
	} else if result.Mode == "wireguard" && result.WireGuard.Connected {
		result.IPv6Only = physicalReady
		if state != nil && state.Phase == "connected" && !result.RecoveryPending {
			verified, message := m.verifyRuntime(ctx, "wireguard", result.Interface, tunnelInterface)
			result.Verified = verified
			if !verified {
				result.Phase = "error"
				result.RecoveryPending = true
				result.Message = joinMessages(result.Message, message)
			}
		}
	} else if result.Mode == "direct" && state != nil && state.Phase == "connected" {
		result.IPv6Only = physicalReady
		verified, message := m.verifyRuntime(ctx, "direct", result.Interface, "")
		result.Verified = verified
		if !verified {
			result.Phase = "error"
			result.RecoveryPending = true
			result.Message = joinMessages(result.Message, message)
		}
	}
	if state != nil && state.Phase == "connected" && !result.RecoveryPending {
		if (result.Mode == "warp" && !result.Warp.Connected) || (result.Mode == "wireguard" && !result.WireGuard.Connected) {
			result.Verified = false
			result.Phase = "error"
			result.RecoveryPending = true
			result.Message = joinMessages(result.Message, "持久化状态显示已连接，但实际隧道已停止")
		}
	}

	if result.RecoveryPending {
		result.Phase = "recovery"
		result.Verified = false
	}
	return result
}

// Connect prepares the selected physical NetworkManager interface and starts
// either Cloudflare WARP or the selected WireGuard profile.  Every mutation is
// preceded by a durable state write; failures trigger best-effort rollback and
// retain a recovery marker if any rollback step fails.
func (m *Manager) Connect(ctx context.Context, mode, ifName, profile string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.active.Store(true)
	defer m.active.Store(false)
	if ctx == nil {
		ctx = context.Background()
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "direct" && mode != "warp" && mode != "wireguard" {
		return fmt.Errorf("不支持的连接模式 %q，可选 direct、warp 或 wireguard", mode)
	}
	if err := validateInterfaceName(ifName); err != nil {
		return err
	}
	if mode == "wireguard" {
		if err := validateProfileName(profile); err != nil {
			return err
		}
	} else if strings.TrimSpace(profile) != "" {
		return fmt.Errorf("%s 模式不接受 WireGuard 配置名", mode)
	}
	if !m.isPrivileged() {
		return errors.New("连接校园网 IPv6 模式需要 root 权限，请用 sudo 或系统服务运行")
	}

	previous, err := m.readState()
	if err != nil {
		return fmt.Errorf("无法读取已有网络状态，拒绝开始新连接：%w", err)
	}
	if previous != nil && (previous.RecoveryPending || previous.Phase == "connecting" || previous.Phase == "disconnecting" || previous.Phase == "recovery") {
		return fmt.Errorf("已有 %s 模式处于 %s 状态，请先点击断开并完成恢复", fallbackText(previous.Mode, "未知"), fallbackText(previous.Phase, "未知"))
	}

	deps := m.dependencies()
	if !dependencyAvailable(deps, m.commands.nmcli) || !dependencyAvailable(deps, m.commands.ip) {
		return errors.New("缺少 nmcli 或 ip，无法安全检查校园网 IPv6 外层网络")
	}
	if mode == "warp" && !dependencyAvailable(deps, m.commands.warpCLI) {
		return errors.New("未找到 warp-cli，请先安装 Cloudflare WARP Linux 客户端")
	}
	if mode == "wireguard" && (!dependencyAvailable(deps, m.commands.wg) || !dependencyAvailable(deps, m.commands.wgQuick)) {
		return errors.New("未找到 wg 或 wg-quick，请先安装 wireguard-tools")
	}
	if !dependencyAvailable(deps, m.commands.curl) {
		return errors.New("未找到 curl，无法验证 IPv6 实际联网结果；拒绝把隧道接口误报为已连接")
	}

	interfaces, interfaceErr := m.collectInterfaces(ctx)
	if interfaceErr != nil {
		return fmt.Errorf("无法读取 NetworkManager 接口：%w", interfaceErr)
	}
	target, ok := findInterface(interfaces, ifName)
	if !ok || !target.Physical || !interfaceStateUsable(target.State) {
		return fmt.Errorf("网卡 %s 不是已连接且由 NetworkManager 管理的物理网卡", ifName)
	}
	if !hasGlobalIPv6(target.IPv6) {
		return fmt.Errorf("网卡 %s 没有公网 IPv6 地址，请先确认校园网 IPv6 已分配", ifName)
	}
	if err := m.checkConflicts(ctx, mode, ifName); err != nil {
		return err
	}
	if err := rejectOtherPhysicalIPv4(interfaces, ifName); err != nil {
		return err
	}

	var profilePath string
	if mode == "wireguard" {
		profilePath, err = m.validateAndResolveProfile(profile)
		if err != nil {
			return err
		}
	} else if mode == "warp" {
		if err := m.preflightWarp(ctx); err != nil {
			return err
		}
	}
	underlay, err := m.captureUnderlay(ctx, ifName, target)
	if err != nil {
		return err
	}
	var desktopProxies []desktopProxySnapshot
	if mode == "warp" {
		desktopProxies, err = m.captureDesktopProxies(ctx)
		if err != nil {
			return err
		}
	}

	state := persistentState{
		Version:         1,
		Mode:            mode,
		Interface:       ifName,
		Profile:         normalizeProfileName(profile),
		Phase:           "connecting",
		RecoveryPending: true,
		UpdatedAt:       time.Now().UTC(),
		Underlay:        underlay,
		DesktopProxies:  desktopProxies,
	}
	if err := m.writeState(state); err != nil {
		return fmt.Errorf("无法在修改网络前保存恢复状态：%w", err)
	}
	if err := m.suspendDesktopProxies(ctx, &state); err != nil {
		// IPv4 has not been touched yet. Do not reapply NetworkManager on a
		// desktop-only failure, but retain the journal if proxy recovery fails.
		state.Underlay.IPv4Disabled = false
		return m.failBeforeTunnel(state, err)
	}

	if underlay.IPv4Disabled {
		if err := m.disableIPv4(ctx, ifName); err != nil {
			return m.failBeforeTunnel(state, fmt.Errorf("无法将 %s 临时切换为 IPv6 外层：%w", ifName, err))
		}
	}

	var startErr error
	switch mode {
	case "direct":
		// The interface is already IPv6-only after the underlay step.
	case "warp":
		startErr = m.startWarp(ctx)
	case "wireguard":
		startErr = m.startWireGuard(ctx, profilePath)
	}
	if startErr != nil {
		return m.rollbackConnection(ctx, state, startErr)
	}

	verifyCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	tunnelName := ""
	if mode == "wireguard" {
		tunnelName = normalizeProfileName(profile)
	}
	verified, verifyMessage := m.waitForVerified(verifyCtx, mode, ifName, tunnelName)
	if !verified {
		return m.rollbackConnection(ctx, state, errors.New(verifyMessage))
	}

	state.Phase = "connected"
	state.RecoveryPending = false
	state.Message = "IPv6 双栈隧道已验证"
	state.UpdatedAt = time.Now().UTC()
	if err := m.writeState(state); err != nil {
		return m.rollbackConnection(ctx, state, fmt.Errorf("连接已建立但无法保存完成状态：%w", err))
	}
	return nil
}

func (m *Manager) isPrivileged() bool {
	if m.privileged != nil {
		return m.privileged()
	}
	return os.Geteuid() == 0
}

// Disconnect tears down the manager-owned tunnel and restores the exact
// NetworkManager IPv4 method captured before Connect.  A failed restore leaves
// recoveryPending=true so a later request can retry instead of silently
// claiming that the host is back to its original state.
func (m *Manager) Disconnect(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.active.Store(true)
	defer m.active.Store(false)
	if ctx == nil {
		ctx = context.Background()
	}
	state, err := m.readState()
	if err != nil {
		return fmt.Errorf("无法读取网络恢复状态：%w", err)
	}
	if state == nil {
		return m.disconnectWithoutState(ctx)
	}
	if state.Phase == "idle" && !state.RecoveryPending {
		return nil
	}
	state.Phase = "disconnecting"
	state.RecoveryPending = true
	state.UpdatedAt = time.Now().UTC()
	if err := m.writeState(*state); err != nil {
		return fmt.Errorf("无法在断开前保存恢复状态：%w", err)
	}

	var failures []string
	tunnelStopped := true
	if state.Mode == "warp" {
		if err := m.stopWarp(ctx); err != nil {
			failures = append(failures, err.Error())
			tunnelStopped = false
		}
	} else if state.Mode == "wireguard" {
		profilePath, resolveErr := m.validateAndResolveProfile(state.Profile)
		if resolveErr != nil {
			if err := m.stopWireGuardByName(ctx, state.Profile); err != nil {
				failures = append(failures, err.Error())
				tunnelStopped = false
			}
		} else if err := m.stopWireGuard(ctx, profilePath); err != nil {
			failures = append(failures, err.Error())
			tunnelStopped = false
		}
	}
	if tunnelStopped && state.Underlay.IPv4Disabled {
		restoreCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		err := m.restoreUnderlay(restoreCtx, state.Underlay)
		cancel()
		if err != nil {
			failures = append(failures, err.Error())
		}
	} else if !tunnelStopped && state.Underlay.IPv4Disabled {
		failures = append(failures, "隧道未被确认关闭，暂不恢复原 IPv4 外层")
	}
	if tunnelStopped {
		restoreCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		if err := m.restoreDesktopProxies(restoreCtx, state.DesktopProxies); err != nil {
			failures = append(failures, err.Error())
		}
		cancel()
	}
	if len(failures) > 0 {
		state.Phase = "recovery"
		state.RecoveryPending = true
		state.Message = strings.Join(failures, "；")
		state.UpdatedAt = time.Now().UTC()
		if writeErr := m.writeState(*state); writeErr != nil {
			state.Message = joinMessages(state.Message, fmt.Sprintf("且无法保存恢复状态：%v", writeErr))
			return fmt.Errorf("断开或恢复原网络失败：%s；保存恢复状态失败：%v", strings.Join(failures, "；"), writeErr)
		}
		return fmt.Errorf("断开或恢复原网络失败：%s", strings.Join(failures, "；"))
	}
	if err := m.removeState(); err != nil {
		return fmt.Errorf("网络已尝试恢复，但无法清理状态文件：%w", err)
	}
	return nil
}

func (m *Manager) disconnectWithoutState(ctx context.Context) error {
	_ = ctx
	// The service calls Disconnect during shutdown. Without a journal there is
	// no proof that a running tunnel belongs to BKNetwork, so this path must be
	// strictly read-only and must never stop an unrelated user's VPN.
	return nil
}

func (m *Manager) failBeforeTunnel(state persistentState, cause error) error {
	restoreCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	var restoreErrors []error
	if state.Underlay.IPv4Disabled {
		if err := m.restoreUnderlay(restoreCtx, state.Underlay); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if err := m.restoreDesktopProxies(restoreCtx, state.DesktopProxies); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	if restoreErr := errors.Join(restoreErrors...); restoreErr != nil {
		state.Phase = "recovery"
		state.RecoveryPending = true
		state.Message = fmt.Sprintf("%v；恢复原网络或系统代理失败：%v", cause, restoreErr)
		state.UpdatedAt = time.Now().UTC()
		if writeErr := m.writeState(state); writeErr != nil {
			return fmt.Errorf("%s；保存恢复状态失败：%v", state.Message, writeErr)
		}
		return errors.New(state.Message)
	}
	if removeErr := m.removeState(); removeErr != nil {
		state.Phase = "error"
		state.RecoveryPending = false
		state.Message = fmt.Sprintf("%v；清理状态文件失败：%v", cause, removeErr)
		state.UpdatedAt = time.Now().UTC()
		if writeErr := m.writeState(state); writeErr != nil {
			return fmt.Errorf("%v；清理状态文件失败：%v；保存错误状态失败：%v", cause, removeErr, writeErr)
		}
		return fmt.Errorf("%v；清理状态文件失败：%v", cause, removeErr)
	}
	return cause
}

func (m *Manager) rollbackConnection(ctx context.Context, state persistentState, cause error) error {
	_ = ctx
	rollbackCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	var failures []string
	tunnelStopped := true
	if state.Mode == "warp" {
		if err := m.stopWarp(rollbackCtx); err != nil {
			failures = append(failures, err.Error())
			tunnelStopped = false
		}
	} else if state.Mode == "wireguard" {
		profilePath, resolveErr := m.validateAndResolveProfile(state.Profile)
		if resolveErr != nil {
			if err := m.stopWireGuardByName(rollbackCtx, state.Profile); err != nil {
				failures = append(failures, err.Error())
				tunnelStopped = false
			}
		} else if err := m.stopWireGuard(rollbackCtx, profilePath); err != nil {
			failures = append(failures, err.Error())
			tunnelStopped = false
		}
	}
	if tunnelStopped && state.Underlay.IPv4Disabled {
		if err := m.restoreUnderlay(rollbackCtx, state.Underlay); err != nil {
			failures = append(failures, err.Error())
		}
	} else if !tunnelStopped && state.Underlay.IPv4Disabled {
		failures = append(failures, "隧道未被确认关闭，暂不恢复原 IPv4 外层")
	}
	if tunnelStopped {
		if err := m.restoreDesktopProxies(rollbackCtx, state.DesktopProxies); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		state.Phase = "recovery"
		state.RecoveryPending = true
		state.Message = joinMessages(cause.Error(), strings.Join(failures, "；"))
		state.UpdatedAt = time.Now().UTC()
		if writeErr := m.writeState(state); writeErr != nil {
			return fmt.Errorf("%v；自动恢复失败：%s；保存恢复状态失败：%v", cause, strings.Join(failures, "；"), writeErr)
		}
		return fmt.Errorf("%v；自动恢复失败：%s", cause, strings.Join(failures, "；"))
	}
	if removeErr := m.removeState(); removeErr != nil {
		state.Phase = "error"
		state.RecoveryPending = false
		state.Message = fmt.Sprintf("%v；清理状态文件失败：%v", cause, removeErr)
		state.UpdatedAt = time.Now().UTC()
		if writeErr := m.writeState(state); writeErr != nil {
			return fmt.Errorf("%v；清理状态文件失败：%v；保存错误状态失败：%v", cause, removeErr, writeErr)
		}
		return fmt.Errorf("%v；清理状态文件失败：%v", cause, removeErr)
	}
	return cause
}

func (m *Manager) startWarp(ctx context.Context) error {
	if _, err := m.run(ctx, m.commands.warpCLI, "connect"); err != nil {
		return fmt.Errorf("WARP 连接命令失败：%w", err)
	}
	return nil
}

func (m *Manager) stopWarp(ctx context.Context) error {
	if !m.commandAvailable(m.commands.warpCLI) {
		return errors.New("未找到 warp-cli，无法安全断开 WARP")
	}
	_, disconnectErr := m.run(ctx, m.commands.warpCLI, "disconnect")
	status := m.probeWarp(ctx)
	// Some client versions report an error when disconnecting an already
	// disconnected tunnel. A definite status is sufficient for safe recovery.
	if warpStatusDefinitelyDisconnected(status.Status) {
		return nil
	}
	if disconnectErr != nil {
		return fmt.Errorf("WARP 断开失败：%w", disconnectErr)
	}
	if status.Connected {
		return errors.New("WARP 断开命令返回成功，但状态仍为 connected")
	}
	if !warpStatusDefinitelyDisconnected(status.Status) {
		return fmt.Errorf("WARP 断开后状态未知（%s），拒绝恢复 IPv4 以免留下未确认的隧道", fallbackText(status.Status, "空白"))
	}
	return nil
}

func warpStatusDefinitelyDisconnected(status string) bool {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch lower {
	case "disconnected", "disabled", "never connected":
		return true
	default:
		return false
	}
}

func (m *Manager) startWireGuard(ctx context.Context, profilePath string) error {
	if _, err := m.run(ctx, m.commands.wgQuick, "up", profilePath); err != nil {
		return fmt.Errorf("WireGuard 启动失败：%w", err)
	}
	return nil
}

func (m *Manager) stopWireGuard(ctx context.Context, profilePath string) error {
	base := strings.TrimSuffix(filepath.Base(profilePath), ".conf")
	if err := validateInterfaceName(base); err != nil {
		return fmt.Errorf("WireGuard 隧道名称无效：%w", err)
	}
	active, err := m.wireGuardInterfaceActive(ctx, base)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	if _, err := m.run(ctx, m.commands.wgQuick, "down", profilePath); err != nil {
		return fmt.Errorf("WireGuard 断开失败：%w", err)
	}
	if stillActive, err := m.wireGuardInterfaceActive(ctx, base); err != nil {
		return err
	} else if stillActive {
		return fmt.Errorf("WireGuard 接口 %s 在 down 命令后仍运行", base)
	}
	return nil
}

func (m *Manager) stopWireGuardByName(ctx context.Context, profile string) error {
	base := normalizeProfileName(profile)
	if err := validateInterfaceName(base); err != nil {
		return fmt.Errorf("WireGuard 恢复接口名称无效：%w", err)
	}
	active, err := m.wireGuardInterfaceActive(ctx, base)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	return fmt.Errorf("WireGuard 配置 %s.conf 无法安全校验且接口仍运行；为避免执行未知 PostDown 脚本，请手动关闭该隧道后重试", base)
}

func (m *Manager) wireGuardInterfaceActive(ctx context.Context, name string) (bool, error) {
	output, err := m.run(ctx, m.commands.wg, "show", "interfaces")
	if err != nil {
		return false, fmt.Errorf("无法确认 WireGuard 接口 %s 是否仍在运行：%w", name, err)
	}
	for _, active := range strings.Fields(output) {
		if active == name {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) run(parent context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	return m.runner.Run(ctx, name, args...)
}

func fallbackText(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func joinMessages(existing, next string) string {
	if strings.TrimSpace(next) == "" {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return next
	}
	return existing + "；" + next
}

func findInterface(interfaces []Interface, name string) (Interface, bool) {
	for _, iface := range interfaces {
		if iface.Name == name {
			return iface, true
		}
	}
	return Interface{}, false
}

func interfaceStateUsable(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "up", "connected", "connecting":
		return true
	default:
		return false
	}
}

func hasGlobalIPv6(addresses []string) bool {
	for _, value := range addresses {
		ip := parseAddress(value)
		if ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func parseAddress(value string) net.IP {
	value = strings.TrimSpace(value)
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	return net.ParseIP(value)
}

func recommendedInterface(interfaces []Interface) string {
	for _, iface := range interfaces {
		if iface.Physical && interfaceStateUsable(iface.State) && hasGlobalIPv6(iface.IPv6) {
			return iface.Name
		}
	}
	return ""
}

func rejectOtherPhysicalIPv4(interfaces []Interface, selected string) error {
	for _, iface := range interfaces {
		if !iface.Physical || iface.Name == selected || !interfaceStateUsable(iface.State) {
			continue
		}
		for _, address := range iface.IPv4 {
			ip := parseAddress(address)
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return fmt.Errorf("检测到另一张物理网卡 %s 仍有可用 IPv4 地址 %s；为避免修改无关网络，请先断开该网卡的 IPv4 出口", iface.Name, address)
			}
		}
		if hasGlobalIPv6(iface.IPv6) {
			return fmt.Errorf("检测到另一张物理网卡 %s 仍有公网 IPv6 出口；无法确认 WARP/WireGuard 使用选定校园网，请先断开该网卡", iface.Name)
		}
	}
	return nil
}

func usableIPv4(addresses []string) []string {
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ip := parseAddress(address)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			result = append(result, address)
		}
	}
	return result
}

func noOtherPhysicalEgress(interfaces []Interface, selected string) bool {
	for _, iface := range interfaces {
		if iface.Name == selected || !iface.Physical || !interfaceStateUsable(iface.State) {
			continue
		}
		if len(usableIPv4(iface.IPv4)) > 0 || hasGlobalIPv6(iface.IPv6) {
			return false
		}
	}
	return true
}
