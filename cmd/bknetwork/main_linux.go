package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bknetwork/internal/appinfo"
	"bknetwork/internal/appproxy"
	"bknetwork/internal/handlers"
	"bknetwork/internal/linuxnet"
	"bknetwork/internal/server"
	"bknetwork/internal/settings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "BKNetwork:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "run"
	if len(args) > 0 && (len(args[0]) == 0 || args[0][0] != '-') {
		command = args[0]
		args = args[1:]
	}
	if command == "app-proxy" {
		return appproxy.Run(args)
	}
	flags := flag.NewFlagSet("bknetwork", flag.ContinueOnError)
	noBrowser := flags.Bool("no-browser", false, "不自动打开浏览器")
	stateDir := flags.String("state-dir", settings.DefaultStateDir(), "设置与恢复记录目录")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "BKNetwork Ubuntu 26.04\n用法: bknetwork [run|status|recover|version] [--no-browser] [--state-dir DIR]\n应用分流: bknetwork app-proxy [install --port 7897|status|remove]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("不支持的位置参数: %v", flags.Args())
	}
	if command == "version" {
		fmt.Println(appinfo.DisplayName + " · Ubuntu 26.04 amd64")
		return nil
	}
	if command != "run" && command != "status" && command != "recover" {
		return fmt.Errorf("未知命令 %q；使用 --help 查看帮助", command)
	}
	if *stateDir == "" || !filepath.IsAbs(*stateDir) {
		return fmt.Errorf("状态目录必须为绝对路径")
	}
	manager := linuxnet.NewManager(*stateDir)
	if command == "status" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(manager.Status(ctx))
	}
	unlock, err := lockInstance(*stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if command == "recover" {
		if os.Geteuid() != 0 {
			return fmt.Errorf("恢复网络需要 sudo ./bknetwork recover")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := manager.Disconnect(ctx); err != nil {
			return err
		}
		fmt.Println("BKNetwork 管理的网络变更已恢复")
		return nil
	}
	store := settings.NewStore(*stateDir)
	if _, err := store.Load(); err != nil {
		return err
	}
	handlers.Configure(manager, store)
	srv := server.NewServer("")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	select {
	case err := <-errCh:
		return err
	case <-srv.Ready():
	}
	log.Printf("%s · http://%s", appinfo.DisplayName, server.DefaultAddr)
	if os.Geteuid() != 0 {
		log.Print("当前为只读预览；网络控制请用 sudo ./bknetwork run --no-browser")
		if !*noBrowser {
			if err := openBrowser("http://" + server.DefaultAddr); err != nil {
				log.Printf("请手动打开管理页面: %v", err)
			}
		}
	}
	serverErr := <-errCh
	if os.Geteuid() == 0 {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := manager.Disconnect(restoreCtx); err != nil {
			return errors.Join(serverErr, fmt.Errorf("网络恢复未完成，请执行 sudo ./bknetwork recover: %w", err))
		}
	}
	return serverErr
}

func openBrowser(address string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil
	}
	cmd := exec.Command("xdg-open", address)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func lockInstance(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("状态目录必须由当前用户所有，且权限为 0700 的普通目录: %s", dir)
	}
	fd, err := syscall.Open(filepath.Join(dir, "instance.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "instance.lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("同一状态目录已有 BKNetwork 运行；请打开 http://%s，或先停止服务再执行 recover", server.DefaultAddr)
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}
