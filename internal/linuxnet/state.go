package linuxnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func (m *Manager) statePath() string {
	return filepath.Join(m.stateDir, stateFileName)
}

func (m *Manager) readState() (*persistentState, error) {
	info, err := os.Lstat(m.stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("状态目录必须是普通目录")
	}
	if info.Mode().Perm()&0o077 != 0 {
		// A freshly-created temporary/config directory may still carry its
		// parent mode before the first journal write.  There is no state to
		// trust yet; writeState will tighten it to 0700 before mutation.
		if _, statErr := os.Lstat(filepath.Join(m.stateDir, stateFileName)); errors.Is(statErr, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("状态目录权限过宽（需要 0700）")
	}
	path := m.statePath()
	fileInfo, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("状态文件不是安全的普通文件")
	}
	if fileInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("状态文件权限过宽（需要 0600）")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state persistentState
	if err := json.Unmarshal(content, &state); err != nil {
		return nil, fmt.Errorf("状态文件格式无效：%w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("状态文件版本 %d 不受支持", state.Version)
	}
	if state.Mode != "direct" && state.Mode != "warp" && state.Mode != "wireguard" {
		return nil, fmt.Errorf("状态文件包含未知连接模式")
	}
	if state.Interface != "" {
		if err := validateInterfaceName(state.Interface); err != nil {
			return nil, fmt.Errorf("状态文件网卡名称无效：%w", err)
		}
	}
	if state.Profile != "" {
		if err := validateProfileName(state.Profile); err != nil {
			return nil, fmt.Errorf("状态文件配置名无效：%w", err)
		}
	}
	seenUsers := make(map[string]bool)
	if len(state.DesktopProxies) > 64 {
		return nil, errors.New("状态文件包含过多的系统代理恢复用户")
	}
	for _, proxy := range state.DesktopProxies {
		if state.Mode != "warp" || !validDesktopIdentity(proxy.UID, proxy.User) || seenUsers[proxy.UID] {
			return nil, errors.New("状态文件包含无效的系统代理恢复用户")
		}
		if proxy.Mode != "none" && proxy.Mode != "manual" && proxy.Mode != "auto" {
			return nil, errors.New("状态文件包含无效的系统代理恢复模式")
		}
		seenUsers[proxy.UID] = true
	}
	return &state, nil
}

func (m *Manager) writeState(state persistentState) error {
	if existing, statErr := os.Lstat(m.stateDir); statErr == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.IsDir() {
			return fmt.Errorf("状态目录不是安全的普通目录")
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(m.stateDir, 0o700); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(m.stateDir)
	if err != nil {
		return err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("状态目录不是安全的 0700 目录")
	}
	path := m.statePath()
	if fileInfo, statErr := os.Lstat(path); statErr == nil {
		if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
			return fmt.Errorf("拒绝覆盖不安全的状态文件")
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.stateDir, ".state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(content, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(m.stateDir)
}

func (m *Manager) removeState() error {
	path := m.statePath()
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("拒绝删除不安全的状态文件")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(m.stateDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
