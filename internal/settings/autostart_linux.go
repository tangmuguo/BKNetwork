package settings

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const UnitName = "bknetwork.service"

func AutoStartEnabled(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "systemctl", "is-enabled", UnitName).Output()
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}

func SetAutoStart(ctx context.Context, enabled bool) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("修改开机启动需要 root 权限")
	}
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=LoadState", "--value", UnitName).Output()
	if err != nil || strings.TrimSpace(string(out)) != "loaded" {
		return fmt.Errorf("请先运行 sudo ./scripts/install.sh 安装 BKNetwork 系统服务")
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	out, err = exec.CommandContext(ctx, "systemctl", action, UnitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("更新开机启动失败: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}
