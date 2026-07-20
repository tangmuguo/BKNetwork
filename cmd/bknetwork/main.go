package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"bknetwork/internal/handlers"
	"bknetwork/internal/server"
	appsettings "bknetwork/internal/settings"

	"github.com/kardianos/service"
)

var logger service.Logger

func main() {
	closeStartupLog := initializeStartupLog()
	defer closeStartupLog()
	log.Printf("BKNetwork v7 process started (pid=%d, args=%q)", os.Getpid(), os.Args[1:])

	runningAsService, err := isServiceProcess()
	if err != nil {
		reportDesktopFailure(fmt.Errorf("检测运行模式失败: %w", err))
		return
	}

	cfg, err := appsettings.Load()
	if err != nil {
		log.Printf("failed to load settings: %v", err)
		cfg = appsettings.Settings{}
	}

	if !runningAsService && !hasStartupNoElevateArg() {
		relaunched, err := ensureElevatedAtStartup()
		if err != nil {
			if errors.Is(err, errElevationCanceled) {
				reportDesktopFailure(errors.New("管理员权限请求已取消；BKNetwork v7 未启动"))
				return
			}
			reportDesktopFailure(fmt.Errorf("请求管理员权限失败: %w", err))
			return
		}
		if relaunched {
			log.Println("elevated child launched; waiting for the v7 local UI")
			if !cfg.SilentStart {
				if err := openRelaunchedDesktopUI(); err != nil {
					reportDesktopFailure(err)
				}
			}
			return
		}
	}

	if err := appsettings.ApplyStartupShortcut(cfg.AutoStart); err != nil {
		log.Printf("failed to sync autostart setting: %v", err)
	}

	svcConfig := &service.Config{
		Name:        "BKNetwork",
		DisplayName: "BKNetwork Service",
		Description: "Background network helper serving a local web UI on localhost:13335",
	}

	prg := &program{settings: cfg}
	svc, err := service.New(prg, svcConfig)
	if err != nil {
		reportDesktopFailure(fmt.Errorf("初始化后台服务失败: %w", err))
		return
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			if err := svc.Install(); err != nil {
				log.Fatalf("install error: %v", err)
			}
			log.Println("service installed")
			return
		case "uninstall":
			if err := svc.Uninstall(); err != nil {
				log.Fatalf("uninstall error: %v", err)
			}
			log.Println("service uninstalled")
			return
		case "run":
			// fallthrough to run in console
		}
	}

	if err := service.Control(svc, "status"); err == nil {
		// likely running as a service manager
	}

	if !runningAsService {
		log.Println("starting desktop mode")
		if err := runDesktopApp(); err != nil {
			reportDesktopFailure(err)
		}
		return
	}

	runErr := svc.Run()
	if runErr != nil {
		if logger != nil {
			logger.Error(runErr)
		} else {
			log.Fatal(runErr)
		}
	}
}

func initializeStartupLog() func() {
	logPaths := make([]string, 0, 2)
	if configDir, err := os.UserConfigDir(); err == nil {
		logPaths = append(logPaths, filepath.Join(configDir, "BKNetwork", "bknetwork-v7.log"))
	}
	if executablePath, err := os.Executable(); err == nil {
		logPaths = append(logPaths, filepath.Join(filepath.Dir(executablePath), "bknetwork-v7.log"))
	}

	for _, logPath := range logPaths {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			continue
		}
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			continue
		}
		log.SetOutput(logFile)
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
		log.Printf("diagnostic log opened at %s", logPath)
		return func() { _ = logFile.Close() }
	}
	return func() {}
}

func hasStartupNoElevateArg() bool {
	for _, arg := range os.Args[1:] {
		if arg == appsettings.StartupNoElevateArg {
			return true
		}
	}
	return false
}

type program struct {
	httpSrv  *server.Server
	settings appsettings.Settings
}

func (p *program) Start(s service.Service) error {
	// Start should not block. Start the server in a goroutine.
	ctx := context.Background()
	p.httpSrv = server.NewServer("")
	go func() {
		if err := p.httpSrv.Start(ctx); err != nil {
			if logger != nil {
				logger.Error(err)
			} else {
				log.Println("server error:", err)
			}
		}
	}()
	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := handlers.ActivateConfiguredChatGPTProxy(); err != nil {
			if logger != nil {
				logger.Warning(err)
			} else {
				log.Printf("ChatGPT Clash routing activation failed: %v", err)
			}
		}
	}()
	if p.settings.WarpAutoStart {
		go func() {
			if err := handlers.StartWarp(); err != nil {
				if logger != nil {
					logger.Warning(err)
				} else {
					log.Printf("warp auto start failed: %v", err)
				}
			}
		}()
	}
	return nil
}

func (p *program) Stop(s service.Service) error {
	// Stop should stop the server gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := handlers.SuspendConfiguredChatGPTProxy(); err != nil {
		if logger != nil {
			logger.Warning(err)
		} else {
			log.Printf("ChatGPT Clash routing restore failed: %v", err)
		}
	}
	if p.httpSrv != nil {
		_ = p.httpSrv.Shutdown(ctx)
	}
	return nil
}
