// Package settings stores Ubuntu application preferences separately from the
// network manager's recovery journal. It never stores WireGuard credentials.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	AutoStart bool `json:"autoStart"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func DefaultStateDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/bknetwork"
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "BKNetwork")
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Load() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cfg Settings
	data, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("读取应用设置失败: %w", err)
	}
	return cfg, nil
}

func (s *Store) Save(cfg Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return fmt.Errorf("找不到配置目录")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(s.dir, "settings.json"))
}
