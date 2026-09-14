package linuxnet

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_=+.-]{0,14}$`)

func validateInterfaceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("网卡名称不能为空")
	}
	if len(name) > 15 || !safeNamePattern.MatchString(name) {
		return fmt.Errorf("网卡名称 %q 不安全或超过 Linux 15 字符限制", name)
	}
	return nil
}

func validateProfileName(profile string) error {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return errorsProfile("WireGuard 配置名不能为空")
	}
	if strings.Contains(profile, "/") || strings.Contains(profile, `\`) || strings.Contains(profile, "..") {
		return errorsProfile("WireGuard 配置名包含非法路径片段")
	}
	if strings.HasSuffix(profile, ".conf") {
		profile = strings.TrimSuffix(profile, ".conf")
	}
	if !safeNamePattern.MatchString(profile) {
		return errorsProfile("WireGuard 配置名只能包含字母、数字、下划线、等号、加号、点和连字符，且最多 15 个字符")
	}
	return nil
}

type profileError string

func (e profileError) Error() string { return string(e) }

func errorsProfile(message string) error { return profileError(message) }

func normalizeProfileName(profile string) string {
	profile = strings.TrimSpace(profile)
	return strings.TrimSuffix(profile, ".conf")
}

func (m *Manager) listProfiles() ([]string, error) {
	if err := m.validateProfileDirectory(); err != nil {
		return []string{}, err
	}
	entries, err := os.ReadDir(m.wireGuardDir)
	if err != nil {
		return []string{}, fmt.Errorf("无法读取 WireGuard 配置目录：%w", err)
	}
	profiles := make([]string, 0, len(entries))
	var rejected []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".conf") {
			continue
		}
		base := strings.TrimSuffix(name, ".conf")
		if validateProfileName(base) != nil {
			rejected = append(rejected, name+"（名称不安全）")
			continue
		}
		path := filepath.Join(m.wireGuardDir, name)
		fileInfo, statErr := os.Lstat(path)
		if statErr != nil {
			rejected = append(rejected, name+"（无法检查文件）")
			continue
		}
		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			rejected = append(rejected, name+"（必须是普通文件且不能是符号链接）")
			continue
		}
		if !rootOwned(fileInfo) {
			rejected = append(rejected, name+"（必须由 root 拥有）")
			continue
		}
		if fileInfo.Mode().Perm()&0o077 != 0 {
			rejected = append(rejected, name+"（需要 0600 权限）")
			continue
		}
		profiles = append(profiles, base)
	}
	sort.Strings(profiles)
	if len(rejected) > 0 {
		return profiles, fmt.Errorf("忽略不安全的 WireGuard 配置：%s", strings.Join(rejected, "、"))
	}
	return profiles, nil
}

func (m *Manager) validateProfileDirectory() error {
	info, err := os.Lstat(m.wireGuardDir)
	if err != nil {
		return fmt.Errorf("无法读取 WireGuard 配置目录 %s：%w", m.wireGuardDir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !rootOwned(info) || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("WireGuard 配置目录必须由 root 所有、权限为 0700，且不能是符号链接：%s", m.wireGuardDir)
	}
	return nil
}

func rootOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func (m *Manager) validateAndResolveProfile(profile string) (string, error) {
	if err := validateProfileName(profile); err != nil {
		return "", err
	}
	if err := m.validateProfileDirectory(); err != nil {
		return "", err
	}
	base := normalizeProfileName(profile)
	path := filepath.Join(m.wireGuardDir, base+".conf")
	cleanDir := filepath.Clean(m.wireGuardDir)
	if filepath.Dir(path) != cleanDir {
		return "", errorsProfile("WireGuard 配置路径越界")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("未找到 WireGuard 配置 %s.conf", base)
		}
		return "", fmt.Errorf("无法检查 WireGuard 配置 %s.conf：%w", base, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("WireGuard 配置 %s.conf 必须是普通文件且不能是符号链接", base)
	}
	if !rootOwned(info) {
		return "", fmt.Errorf("WireGuard 配置 %s.conf 必须由 root 拥有", base)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("WireGuard 配置 %s.conf 需要 0600 权限", base)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("无法读取 WireGuard 配置 %s.conf：%w", base, err)
	}
	if err := validateWireGuardConfig(string(content)); err != nil {
		return "", fmt.Errorf("WireGuard 配置 %s.conf 不安全：%w", base, err)
	}
	if configHasDNS(string(content)) && !m.commandAvailable(m.commands.resolvconf) {
		return "", fmt.Errorf("WireGuard 配置 %s.conf 包含 DNS，但系统没有 resolvconf 兼容接口；请确认 systemd-resolved 已安装并提供 /usr/sbin/resolvconf", base)
	}
	return path, nil
}

func validateWireGuardConfig(raw string) error {
	section := ""
	hasInterface := false
	hasPeer := false
	hasV4Default := false
	hasV6Default := false
	endpointCount := 0
	for lineNumber, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(strings.SplitN(rawLine, "#", 2)[0])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
				hasInterface = true
			case "peer":
				hasPeer = true
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("第 %d 行缺少等号", lineNumber+1)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "preup", "postup", "predown", "postdown":
			return fmt.Errorf("第 %d 行包含可执行脚本钩子，BKNetwork 为避免执行任意命令不接受此配置", lineNumber+1)
		case "saveconfig":
			if !strings.EqualFold(value, "false") {
				return fmt.Errorf("第 %d 行 SaveConfig 只允许 false，以免覆盖受保护配置", lineNumber+1)
			}
		case "table":
			if value != "" && !strings.EqualFold(value, "auto") {
				return fmt.Errorf("第 %d 行 Table 必须为 auto，才能验证完整双栈默认路由", lineNumber+1)
			}
		}
		if section == "peer" && key == "endpoint" {
			endpointCount++
			if err := validatePublicIPv6Endpoint(value); err != nil {
				return fmt.Errorf("第 %d 行 endpoint 必须是公网 IPv6 字面量：%w", lineNumber+1, err)
			}
		}
		if section == "peer" && key == "allowedips" {
			for _, item := range strings.Split(value, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				_, network, err := net.ParseCIDR(item)
				if err != nil {
					return fmt.Errorf("第 %d 行 AllowedIPs 含无效网段", lineNumber+1)
				}
				if network.IP.To4() != nil && network.String() == "0.0.0.0/0" {
					hasV4Default = true
				}
				if network.IP.To4() == nil && network.String() == "::/0" {
					hasV6Default = true
				}
			}
		}
	}
	if !hasInterface || !hasPeer {
		return errorsProfile("必须同时包含 [Interface] 和至少一个 [Peer]")
	}
	if endpointCount == 0 {
		return errorsProfile("至少需要一个 Peer Endpoint")
	}
	if !hasV4Default || !hasV6Default {
		return errorsProfile("AllowedIPs 必须同时包含 0.0.0.0/0 与 ::/0，才能验证完整双栈隧道")
	}
	return nil
}

func configHasDNS(raw string) bool {
	section := ""
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && section == "interface" && strings.EqualFold(strings.TrimSpace(key), "dns") && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func validatePublicIPv6Endpoint(endpoint string) error {
	if !strings.HasPrefix(endpoint, "[") {
		return errorsProfile("Endpoint 必须使用 [IPv6]:端口格式")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return errorsProfile("Endpoint 端口格式无效")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return errorsProfile("Endpoint 端口必须是 1-65535")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() {
		return errorsProfile("Endpoint 必须是公网 IPv6 字面量，不能使用主机名、IPv4 或内网地址")
	}
	return nil
}
