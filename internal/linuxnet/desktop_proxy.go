package linuxnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"bknetwork/internal/appproxy"
)

// Only the GNOME mode is changed. Proxy endpoints, PAC URLs, and bypass lists
// remain owned by Clash/the desktop user and are never copied into our journal.
type desktopProxySnapshot struct {
	UID     string `json:"uid"`
	User    string `json:"user"`
	Mode    string `json:"mode"`
	Restore bool   `json:"restore,omitempty"`
}

type desktopUser struct {
	UID, Name, Home string
}

func validDesktopIdentity(uid, name string) bool {
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != uid || name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "\x00\n\r:/ ")
}

func proxyMode(raw string) (string, error) {
	// gsettings writes diagnostics (for example a dconf warning) to stderr.
	// commandRunner intentionally returns combined stdout/stderr, so accept a
	// standalone mode line instead of requiring the entire output to be one
	// token.  Keep the accepted values exact; never infer a mode from free text.
	for _, line := range strings.Split(raw, "\n") {
		value := strings.TrimSpace(line)
		for _, mode := range []string{"none", "manual", "auto"} {
			if value == "'"+mode+"'" {
				return mode, nil
			}
		}
	}
	return "", errors.New("无法识别 GNOME 系统代理模式")
}

func desktopSession(raw string) (desktopUser, bool) {
	values := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	u := desktopUser{UID: values["User"], Name: values["Name"]}
	return u, values["Active"] == "yes" && values["Remote"] == "no" &&
		(values["Type"] == "wayland" || values["Type"] == "x11") && validDesktopIdentity(u.UID, u.Name)
}

func (m *Manager) activeDesktopUsers(ctx context.Context) ([]desktopUser, error) {
	if m.desktopUserProvider != nil {
		return m.desktopUserProvider(ctx)
	}
	if !m.commandAvailable("loginctl") {
		return nil, nil // Headless installations have no desktop proxy to own.
	}
	raw, err := m.run(ctx, "loginctl", "list-sessions", "--no-legend", "--no-pager")
	if err != nil {
		return nil, errors.New("无法枚举桌面会话，不能安全保存系统代理")
	}
	var users []desktopUser
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Session IDs are passed as individual arguments, never shell source.
		id := fields[0]
		if strings.HasPrefix(id, "-") || strings.ContainsAny(id, "/\x00") {
			return nil, errors.New("loginctl 返回无效的会话标识")
		}
		detail, err := m.run(ctx, "loginctl", "show-session", id,
			"--property=User", "--property=Name", "--property=Type", "--property=Remote", "--property=Active")
		if err != nil {
			return nil, errors.New("无法读取桌面会话，不能安全保存系统代理")
		}
		u, active := desktopSession(detail)
		if !active || seen[u.UID] {
			continue
		}
		account, err := m.lookupDesktopAccount(u.UID)
		if err != nil || account == nil || account.Uid != u.UID || account.Username != u.Name || !filepath.IsAbs(account.HomeDir) || account.HomeDir == "/" {
			return nil, errors.New("桌面会话与本机用户信息不一致")
		}
		u.Home = account.HomeDir
		users = append(users, u)
		seen[u.UID] = true
	}
	return users, nil
}

func (m *Manager) lookupDesktopAccount(uid string) (*user.User, error) {
	if m.desktopAccountLookup != nil {
		return m.desktopAccountLookup(uid)
	}
	return user.LookupId(uid)
}

func (m *Manager) desktopProxyCommand(ctx context.Context, snapshot desktopProxySnapshot, action string, value ...string) (string, error) {
	if !validDesktopIdentity(snapshot.UID, snapshot.User) {
		return "", errors.New("系统代理恢复记录的用户身份无效")
	}
	account, err := m.lookupDesktopAccount(snapshot.UID)
	if err != nil || account == nil || account.Uid != snapshot.UID || account.Username != snapshot.User {
		return "", errors.New("系统代理恢复用户已经改变，拒绝访问其他用户的桌面")
	}
	// runuser drops root before accessing dconf. Explicit session-bus variables
	// avoid changing root's settings or inheriting the service's D-Bus context.
	args := []string{"--user", snapshot.User, "--", "/usr/bin/env",
		"-u", "DCONF_PROFILE", "-u", "GSETTINGS_SCHEMA_DIR", "-u", "XDG_CONFIG_HOME",
		"GSETTINGS_BACKEND=dconf",
		"XDG_RUNTIME_DIR=/run/user/" + snapshot.UID,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + snapshot.UID + "/bus",
	}
	// Run through the target user's systemd --user manager when available. It
	// supplies the same desktop-session environment as a terminal-launched
	// gsettings process, including any per-user dconf profile. The direct
	// command remains a compatibility fallback for headless/minimal systems.
	if m.commandAvailable("systemd-run") {
		args = append(args, "/usr/bin/systemd-run", "--user", "--wait", "--pipe", "--quiet", "--collect")
	}
	args = append(args, "/usr/bin/gsettings", action, "org.gnome.system.proxy", "mode")
	args = append(args, value...)
	return m.run(ctx, "runuser", args...)
}

// The local Clash guard would undo our mode change. Read only this boolean;
// never log the file, which can also contain private subscription information.
func clashProxyGuardEnabled(home string) (bool, error) {
	path := filepath.Join(home, ".local", "share", "io.github.clash-verge-rev.clash-verge-rev", "verge.yaml")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("无法检查 Clash Verge 的系统代理守卫设置")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("Clash Verge 设置必须是普通文件")
	}
	data, err := io.ReadAll(io.LimitReader(f, 128<<10))
	if err != nil || len(data) == 128<<10 {
		return false, errors.New("无法检查 Clash Verge 的系统代理守卫设置")
	}
	found, enabled := false, false
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.Trim(strings.TrimSpace(key), "\"'") != "enable_proxy_guard" {
			continue
		}
		if found {
			return false, errors.New("Clash Verge 系统代理守卫设置重复，无法确认是否启用")
		}
		found = true
		value, _, _ = strings.Cut(value, "#")
		switch strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'")) {
		case "true", "yes", "on", "1":
			enabled = true
		case "false", "no", "off", "0", "null", "~", "":
			enabled = false
		default:
			return false, errors.New("无法识别 Clash Verge 的系统代理守卫设置")
		}
	}
	return enabled, nil
}

func (m *Manager) captureDesktopProxies(ctx context.Context) ([]desktopProxySnapshot, error) {
	users, err := m.activeDesktopUsers(ctx)
	if err != nil {
		return nil, err
	}
	var result []desktopProxySnapshot
	for _, u := range users {
		cfg, err := appproxy.ReadConfig(u.Home)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取用户 %s 的 Clash 应用分流配置失败：%w", u.Name, err)
		}
		if cfg == nil {
			continue
		}
		for _, command := range []string{"runuser", "gsettings"} {
			if !m.commandAvailable(command) {
				return nil, fmt.Errorf("Clash 应用分流需要 %s", command)
			}
		}
		guard, err := clashProxyGuardEnabled(u.Home)
		if err != nil {
			return nil, err
		}
		if guard {
			return nil, errors.New("请先关闭 Clash Verge 的系统代理守卫，否则它会在 WARP 连接后重新接管其他应用")
		}
		snapshot := desktopProxySnapshot{UID: u.UID, User: u.Name}
		raw, err := m.desktopProxyCommand(ctx, snapshot, "get")
		if err != nil {
			return nil, fmt.Errorf("无法读取用户 %s 的 GNOME 系统代理：%w", u.Name, err)
		}
		snapshot.Mode, err = proxyMode(raw)
		if err != nil {
			return nil, fmt.Errorf("无法解析用户 %s 的 GNOME 系统代理：%w", u.Name, err)
		}
		result = append(result, snapshot)
	}
	return result, nil
}

func (m *Manager) suspendDesktopProxies(ctx context.Context, state *persistentState) error {
	for index := range state.DesktopProxies {
		snapshot := &state.DesktopProxies[index]
		raw, err := m.desktopProxyCommand(ctx, *snapshot, "get")
		mode, parseErr := proxyMode(raw)
		if err != nil || parseErr != nil || mode != snapshot.Mode {
			if err != nil {
				return fmt.Errorf("无法重新读取用户 %s 的 GNOME 系统代理：%w", snapshot.User, err)
			}
			if parseErr != nil {
				return fmt.Errorf("无法解析用户 %s 的 GNOME 系统代理：%w", snapshot.User, parseErr)
			}
			return fmt.Errorf("用户 %s 的系统代理在连接准备期间从 %s 变为 %s，请保持设置稳定后重试", snapshot.User, snapshot.Mode, mode)
		}
		if snapshot.Mode != "none" {
			// Arm recovery immediately before this user's mutation. Captured
			// entries not reached yet must not undo newer user choices on abort.
			snapshot.Restore = true
			if err := m.writeState(*state); err != nil {
				snapshot.Restore = false
				return fmt.Errorf("无法在暂停系统代理前保存恢复记录：%w", err)
			}
			if _, err := m.desktopProxyCommand(ctx, *snapshot, "set", "none"); err != nil {
				return fmt.Errorf("暂停用户 %s 的系统代理失败", snapshot.User)
			}
		}
		raw, err = m.desktopProxyCommand(ctx, *snapshot, "get")
		mode, parseErr = proxyMode(raw)
		if err != nil || parseErr != nil || mode != "none" {
			if err != nil {
				return fmt.Errorf("无法确认用户 %s 的 GNOME 系统代理已暂停：%w", snapshot.User, err)
			}
			if parseErr != nil {
				return fmt.Errorf("无法解析用户 %s 的 GNOME 系统代理暂停状态：%w", snapshot.User, parseErr)
			}
			return fmt.Errorf("用户 %s 的系统代理未保持暂停（当前为 %s），请检查 Clash 的系统代理守卫", snapshot.User, mode)
		}
	}
	return nil
}

func (m *Manager) restoreDesktopProxies(ctx context.Context, snapshots []desktopProxySnapshot) error {
	var failures []error
	for _, snapshot := range snapshots {
		if !snapshot.Restore || snapshot.Mode == "none" {
			continue // Nothing to restore; do not invent a previous proxy mode.
		}
		raw, err := m.desktopProxyCommand(ctx, snapshot, "get")
		mode, parseErr := proxyMode(raw)
		if err != nil || parseErr != nil {
			failures = append(failures, fmt.Errorf("无法读取用户 %s 的系统代理以进行恢复", snapshot.User))
			continue
		}
		if mode != "none" {
			// Already restored, or deliberately changed by the user since we
			// paused it. Respect the user's newer setting in either case.
			continue
		}
		if _, err := m.desktopProxyCommand(ctx, snapshot, "set", snapshot.Mode); err != nil {
			failures = append(failures, fmt.Errorf("恢复用户 %s 的系统代理失败", snapshot.User))
			continue
		}
		raw, err = m.desktopProxyCommand(ctx, snapshot, "get")
		mode, parseErr = proxyMode(raw)
		if err != nil || parseErr != nil || mode != snapshot.Mode {
			failures = append(failures, fmt.Errorf("用户 %s 的系统代理尚未确认恢复", snapshot.User))
		}
	}
	return errors.Join(failures...)
}
