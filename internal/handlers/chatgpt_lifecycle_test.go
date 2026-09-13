package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatGPTProxyCannotReactivateDuringShutdown(t *testing.T) {
	// Broken settings make any unexpected settings read observable. Neither
	// a delayed startup nor a late POST may touch system settings after exit.
	configDir := t.TempDir()
	t.Setenv("APPDATA", configDir)
	if err := os.MkdirAll(filepath.Join(configDir, "BKNetwork"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "BKNetwork", "settings.json"), []byte("invalid json"), 0600); err != nil {
		t.Fatal(err)
	}
	chatGPTProxyMu.Lock()
	previous := chatGPTProxyStopping
	chatGPTProxyStopping = true
	chatGPTProxyMu.Unlock()
	t.Cleanup(func() {
		chatGPTProxyMu.Lock()
		chatGPTProxyStopping = previous
		chatGPTProxyMu.Unlock()
	})
	if err := ActivateConfiguredChatGPTProxy(); err != nil {
		t.Fatalf("delayed startup touched settings: %v", err)
	}
	if err := configureChatGPTProxy(true, "127.0.0.1:7897"); err == nil || !strings.Contains(err.Error(), "正在退出") {
		t.Fatalf("late configuration was not rejected: %v", err)
	}
}
