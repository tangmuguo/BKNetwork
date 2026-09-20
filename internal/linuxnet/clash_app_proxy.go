package linuxnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bknetwork/internal/appproxy"
)

// ClashAppProxyStatus describes the per-user application wrapper state without
// exposing desktop usernames or any proxy credentials.
type ClashAppProxyStatus struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
}

type appProxyChange struct {
	User   desktopUser
	Port   int
	Action string
}

// ClashAppProxyStatus reports whether an active local desktop user has opted
// into the Clash wrappers.  The root service intentionally only considers
// active local sessions, matching the proxy lifecycle used by WARP.
func (m *Manager) ClashAppProxyStatus(ctx context.Context) (ClashAppProxyStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	users, err := m.activeDesktopUsers(ctx)
	if err != nil {
		return ClashAppProxyStatus{}, err
	}
	var result ClashAppProxyStatus
	for _, user := range users {
		config, readErr := appproxy.ReadConfig(user.Home)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return ClashAppProxyStatus{}, fmt.Errorf("读取用户 %s 的 Clash 应用兼容配置失败：%w", user.Name, readErr)
		}
		if !result.Enabled {
			result.Enabled = true
			result.Port = config.Port
			continue
		}
		// Multiple active desktop users should normally share one Clash port. A
		// zero port makes the mismatch visible without leaking either identity.
		if result.Port != config.Port {
			result.Port = 0
		}
	}
	return result, nil
}

// SetClashAppProxy installs or removes the application wrappers for every
// active local desktop user. It runs the existing app-proxy transaction as the
// target user, so generated files keep the user's ownership and environment.
// A tunnel/recovery operation must be settled before changing the wrappers.
func (m *Manager) SetClashAppProxy(ctx context.Context, enabled bool, port int) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if !m.isPrivileged() {
		return errors.New("修改 Clash 应用兼容需要 root 权限")
	}
	if enabled && !validClashAppProxyPort(port) {
		return fmt.Errorf("Clash HTTP/mixed 端口必须在 1-65535 之间")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	state, err := m.readState()
	if err != nil {
		return fmt.Errorf("无法读取网络恢复状态：%w", err)
	}
	if state != nil && (state.RecoveryPending || state.Phase == "connecting" || state.Phase == "connected" || state.Phase == "disconnecting" || state.Phase == "recovery") {
		return errors.New("隧道或网络恢复正在使用中，请先断开并完成恢复后再切换 Clash 应用兼容")
	}

	users, err := m.activeDesktopUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return errors.New("没有活动的本地 GNOME 桌面用户，无法切换 Clash 应用兼容")
	}
	if !m.commandAvailable("runuser") {
		return errors.New("Clash 应用兼容需要 runuser")
	}
	binary, err := m.appProxyExecutable()
	if err != nil {
		return err
	}

	var changes []appProxyChange
	fail := func(cause error) error {
		rollbackErr := m.rollbackAppProxyChanges(ctx, binary, changes)
		if rollbackErr != nil {
			return errors.Join(cause, rollbackErr)
		}
		return cause
	}

	for _, user := range users {
		config, readErr := appproxy.ReadConfig(user.Home)
		if enabled {
			if readErr == nil {
				if config.Port != port {
					return fail(fmt.Errorf("用户 %s 已使用 Clash 端口 %d，请先关闭兼容后再切换到端口 %d", user.Name, config.Port, port))
				}
				continue
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return fail(fmt.Errorf("读取用户 %s 的 Clash 应用兼容配置失败：%w", user.Name, readErr))
			}
			if output, runErr := m.runAppProxyCommand(ctx, binary, user, "install", "--port", strconv.Itoa(port)); runErr != nil {
				return fail(formatAppProxyCommandError(user.Name, "启用", output, runErr))
			}
			changes = append(changes, appProxyChange{User: user, Port: port, Action: "installed"})
			continue
		}

		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return fail(fmt.Errorf("读取用户 %s 的 Clash 应用兼容配置失败：%w", user.Name, readErr))
		}
		if output, runErr := m.runAppProxyCommand(ctx, binary, user, "remove"); runErr != nil {
			return fail(formatAppProxyCommandError(user.Name, "关闭", output, runErr))
		}
		changes = append(changes, appProxyChange{User: user, Port: config.Port, Action: "removed"})
	}
	return nil
}

func validClashAppProxyPort(port int) bool { return port >= 1 && port <= 65535 }

func (m *Manager) appProxyExecutable() (string, error) {
	if value := strings.TrimSpace(m.appProxyBinary); value != "" {
		if !filepath.IsAbs(value) {
			return "", errors.New("BKNetwork app-proxy 可执行文件路径必须是绝对路径")
		}
		return filepath.Clean(value), nil
	}
	value, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法确定 BKNetwork 可执行文件：%w", err)
	}
	if !filepath.IsAbs(value) {
		value, err = filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("无法确定 BKNetwork 可执行文件：%w", err)
		}
	}
	return filepath.Clean(value), nil
}

func (m *Manager) runAppProxyCommand(ctx context.Context, binary string, user desktopUser, action string, args ...string) (string, error) {
	if !validDesktopIdentity(user.UID, user.Name) || !filepath.IsAbs(user.Home) || user.Home == "/" {
		return "", errors.New("活动桌面用户身份无效")
	}
	runArgs := []string{
		"--user", user.Name, "--", "/usr/bin/env",
		"HOME=" + user.Home,
		"XDG_CONFIG_HOME=" + filepath.Join(user.Home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(user.Home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(user.Home, ".local", "state"),
		"PATH=/usr/local/bin:/usr/bin:/bin",
		binary, "app-proxy", action,
	}
	runArgs = append(runArgs, args...)
	return m.run(ctx, "runuser", runArgs...)
}

func formatAppProxyCommandError(user, action, output string, runErr error) error {
	detail := strings.TrimSpace(output)
	if len(detail) > 512 {
		detail = detail[:512] + "…"
	}
	if detail != "" {
		return fmt.Errorf("%s用户 %s 的 Clash 应用兼容失败：%s", action, user, detail)
	}
	return fmt.Errorf("%s用户 %s 的 Clash 应用兼容失败：%w", action, user, runErr)
}

func (m *Manager) rollbackAppProxyChanges(ctx context.Context, binary string, changes []appProxyChange) error {
	var failures []error
	for index := len(changes) - 1; index >= 0; index-- {
		change := changes[index]
		action := "remove"
		args := []string(nil)
		if change.Action == "removed" {
			action = "install"
			args = []string{"--port", strconv.Itoa(change.Port)}
		}
		if output, err := m.runAppProxyCommand(ctx, binary, change.User, action, args...); err != nil {
			failures = append(failures, formatAppProxyCommandError(change.User.Name, "回滚", output, err))
		}
	}
	return errors.Join(failures...)
}
