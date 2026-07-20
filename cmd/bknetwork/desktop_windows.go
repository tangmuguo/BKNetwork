//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"bknetwork/internal/handlers"
	"bknetwork/internal/server"
	appsettings "bknetwork/internal/settings"

	"github.com/kardianos/service"

	"golang.org/x/sys/windows"
	winsvc "golang.org/x/sys/windows/svc"
)

const (
	trayMessageID      = 0x0400 + 1
	trayMenuOpen       = 1001
	trayMenuExit       = 1002
	wmCommand          = 0x0111
	wmClose            = 0x0010
	wmDestroy          = 0x0002
	wmLButtonUp        = 0x0202
	wmRButtonUp        = 0x0205
	wmUser             = 0x0400
	wmNull             = 0x0000
	mbOK               = 0x00000000
	mbIconError        = 0x00000010
	imageIcon          = 1
	lrFromFile         = 0x00000010
	lrDefaultSize      = 0x00000040
	nimAdd             = 0
	nimModify          = 1
	nimDelete          = 2
	nifMessage         = 0x00000001
	nifIcon            = 0x00000002
	nifTip             = 0x00000004
	mfString           = 0x00000000
	mfSeparator        = 0x00000800
	tpmRightBtn        = 0x0002
	tpmBottom          = 0x0020
	swHide             = 0
	wsOverlapped       = 0x00000000
	wsCaption          = 0x00C00000
	wsSysMenu          = 0x00080000
	wsThickFrame       = 0x00040000
	wsMinBox           = 0x00020000
	wsMaxBox           = 0x00010000
	wsOverlappedWindow = wsOverlapped | wsCaption | wsSysMenu | wsThickFrame | wsMinBox | wsMaxBox
	csHRedraw          = 0x0002
	csVRedraw          = 0x0001
	cwUseDefault       = 0x80000000
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procLoadImageW          = user32.NewProc("LoadImageW")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procFreeConsole         = kernel32.NewProc("FreeConsole")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
)

func init() {
	if service.Interactive() {
		hideConsoleWindow()
	}
}

type trayController struct {
	hwnd       windows.Handle
	hicon      windows.Handle
	menu       windows.Handle
	iconPath   string
	browserURL string
	onExit     func()
	onOpen     func()
	cleanup    sync.Once
	className  *uint16
	windowName *uint16
}

type point struct {
	X int32
	Y int32
}

type msg struct {
	hWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	CbSize     uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type notifyIconData struct {
	CbSize           uint32
	HWND             windows.Handle
	UID              uint32
	Flags            uint32
	CallbackMessage  uint32
	HIcon            windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GuidItem         windows.GUID
	BalloonIcon      windows.Handle
}

func runDesktopApp() error {
	cfg, err := appsettings.Load()
	if err != nil {
		log.Printf("failed to load settings: %v", err)
		cfg = appsettings.Settings{}
	}

	trayIcon, err := resolveWebAssetPath("favicon.ico")
	if err != nil {
		log.Printf("tray icon unavailable; continuing without it: %v", err)
		trayIcon = ""
	}

	srv := server.NewServer("")
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start(context.Background())
	}()
	select {
	case <-srv.Ready():
		// This process owns 127.0.0.1:13335. It is now safe to open the UI.
		log.Printf("v7 local UI is listening at http://%s/?v=7", server.DefaultAddr)
	case startErr := <-serverErr:
		message := "BKNetwork v7 无法启动：127.0.0.1:13335 已被占用。\n\n请先从系统托盘退出旧版 BKNetwork，再重新运行 v7 文件夹中的 bknetwork.exe。"
		return fmt.Errorf("%s: %w", message, startErr)
	case <-time.After(10 * time.Second):
		message := "BKNetwork v7 后台启动超时，请退出所有旧版 BKNetwork 后重试。"
		return errors.New(message)
	}
	go func() {
		if err := handlers.ActivateConfiguredChatGPTProxy(); err != nil {
			log.Printf("ChatGPT Clash routing activation failed: %v", err)
		}
	}()

	var once sync.Once
	shutdownServer := func() {
		once.Do(func() {
			if err := handlers.SuspendConfiguredChatGPTProxy(); err != nil {
				log.Printf("ChatGPT Clash routing restore failed: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}

	tray := &trayController{
		iconPath:   trayIcon,
		browserURL: "http://" + server.DefaultAddr + "/?v=7",
		onExit:     shutdownServer,
	}
	if !cfg.SilentStart && !hasElevatedChildArg() {
		tray.onOpen = func() {
			if err := waitForV7UI(10 * time.Second); err != nil {
				log.Printf("auto open browser skipped: %v", err)
				return
			}
			tray.openBrowser()
		}
	}
	if cfg.WarpAutoStart {
		go func() {
			if err := handlers.StartWarp(); err != nil {
				log.Printf("warp auto start failed: %v", err)
			}
		}()
	}
	if tray.onOpen != nil {
		go tray.onOpen()
	}
	if err := tray.run(); err != nil {
		// The HTTP UI is the primary control surface. Keep it alive even if the
		// notification-area shell is temporarily unavailable on this desktop.
		log.Printf("tray unavailable; continuing in browser-only mode: %v", err)
		serverExitErr := <-serverErr
		if serverExitErr != nil && !errors.Is(serverExitErr, http.ErrServerClosed) {
			return fmt.Errorf("browser-only server exited: %w", serverExitErr)
		}
		return nil
	}
	shutdownServer()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server exited: %v", err)
		}
	default:
	}
	return nil
}

func isServiceProcess() (bool, error) {
	return winsvc.IsWindowsService()
}

func openRelaunchedDesktopUI() error {
	if err := waitForV7UI(20 * time.Second); err != nil {
		return fmt.Errorf("管理员进程未能启动 v7 控制页面: %w；请查看 exe 同目录或 %%APPDATA%%\\BKNetwork 下的 bknetwork-v7.log", err)
	}
	if err := openBrowserURL("http://" + server.DefaultAddr + "/?v=7"); err != nil {
		return fmt.Errorf("v7 后台已启动，但打开默认浏览器失败: %w；请手动访问 http://%s/?v=7", err, server.DefaultAddr)
	}
	log.Println("v7 browser launch requested from the non-elevated parent")
	return nil
}

func hideConsoleWindow() {
	procFreeConsole.Call()
}

func showDesktopError(title, message string) {
	titlePtr, titleErr := windows.UTF16PtrFromString(title)
	messagePtr, messageErr := windows.UTF16PtrFromString(message)
	if titleErr != nil || messageErr != nil {
		log.Printf("%s: %s", title, message)
		return
	}
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(messagePtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		mbOK|mbIconError,
	)
}

func reportDesktopFailure(err error) {
	if err == nil {
		return
	}
	log.Printf("desktop startup failed: %v", err)
	showDesktopError(
		"BKNetwork v7 启动失败",
		"BKNetwork v7 未能完成启动。\n\n错误："+err.Error()+"\n\n请确认 exe 与 web 文件夹位于同一目录。",
	)
}

func resolveWebAssetPath(name string) (string, error) {
	search := []string{}
	if exePath, err := os.Executable(); err == nil {
		search = append(search, filepath.Join(filepath.Dir(exePath), "web", name))
	}
	if cwd, err := os.Getwd(); err == nil {
		search = append(search, filepath.Join(cwd, "web", name))
	}
	for _, candidate := range search {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("asset not found: %s", name)
}

func (t *trayController) run() error {
	runtimeLockThread()
	defer runtimeUnlockThread()

	if t.iconPath != "" {
		iconHandle, err := t.loadIcon()
		if err != nil {
			log.Printf("load tray icon failed; continuing without tray icon: %v", err)
		} else {
			t.hicon = iconHandle
			defer procDestroyIcon.Call(uintptr(t.hicon))
		}
	}

	if err := t.registerWindowClass(); err != nil {
		return fmt.Errorf("register tray window class: %w", err)
	}
	defer t.unregisterWindowClass()

	if err := t.createWindow(); err != nil {
		return fmt.Errorf("create tray window: %w", err)
	}
	defer t.destroyWindow()

	trayIconAdded := false
	if t.hicon != 0 {
		if err := t.addTrayIcon(); err != nil {
			log.Printf("add notification-area icon failed; browser UI remains available: %v", err)
		} else {
			trayIconAdded = true
		}
	}
	if trayIconAdded {
		defer t.removeTrayIcon()
	}

	if err := t.createMenu(); err != nil {
		log.Printf("create tray menu failed; browser UI remains available: %v", err)
	} else {
		defer t.destroyMenu()
	}

	msgLoop := msg{}
	for {
		ret, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&msgLoop)), 0, 0, 0)
		if int32(ret) == -1 {
			return fmt.Errorf("read tray window message: %w", err)
		}
		if ret == 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msgLoop)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msgLoop)))
	}
}

func (t *trayController) loadIcon() (windows.Handle, error) {
	iconPtr, err := windows.UTF16PtrFromString(t.iconPath)
	if err != nil {
		return 0, err
	}
	res, _, callErr := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(iconPtr)),
		imageIcon,
		0,
		0,
		lrFromFile|lrDefaultSize,
	)
	if res == 0 {
		return 0, callErr
	}
	return windows.Handle(res), nil
}

func (t *trayController) registerWindowClass() error {
	className, err := windows.UTF16PtrFromString("BKNetworkTrayWindow")
	if err != nil {
		return err
	}
	windowName, err := windows.UTF16PtrFromString("")
	if err != nil {
		return err
	}
	t.className = className
	t.windowName = windowName

	instance, _, err := procGetModuleHandleW.Call(0)
	if instance == 0 {
		return err
	}
	trayWindow = t
	cls := wndClassEx{
		CbSize:     uint32(unsafe.Sizeof(wndClassEx{})),
		Style:      csHRedraw | csVRedraw,
		WndProc:    syscall.NewCallback(trayWndProc),
		Instance:   windows.Handle(instance),
		Background: windows.Handle(6),
		ClassName:  t.className,
		Icon:       t.hicon,
		Cursor:     windows.Handle(0),
		IconSm:     t.hicon,
	}
	res, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls)))
	if res == 0 {
		return callErr
	}
	return nil
}

func (t *trayController) unregisterWindowClass() {}

func (t *trayController) createWindow() error {
	instance, _, err := procGetModuleHandleW.Call(0)
	if instance == 0 {
		return err
	}
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(t.className)),
		uintptr(unsafe.Pointer(t.windowName)),
		uintptr(wsOverlappedWindow),
		uintptr(cwUseDefault),
		uintptr(cwUseDefault),
		uintptr(cwUseDefault),
		uintptr(cwUseDefault),
		0,
		0,
		uintptr(instance),
		0,
	)
	if hwnd == 0 {
		return callErr
	}
	t.hwnd = windows.Handle(hwnd)
	procShowWindow.Call(uintptr(t.hwnd), swHide)
	return nil
}

func (t *trayController) destroyWindow() {
	if t.hwnd != 0 {
		procDestroyWindow.Call(uintptr(t.hwnd))
	}
}

func (t *trayController) addTrayIcon() error {
	nid := notifyIconData{CbSize: uint32(unsafe.Sizeof(notifyIconData{})), HWND: t.hwnd, UID: 1, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: trayMessageID, HIcon: t.hicon}
	t.setTip(&nid, "BKNetwork v7")
	res, _, callErr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if res == 0 {
		return callErr
	}
	return nil
}

func (t *trayController) removeTrayIcon() {
	nid := notifyIconData{CbSize: uint32(unsafe.Sizeof(notifyIconData{})), HWND: t.hwnd, UID: 1}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (t *trayController) createMenu() error {
	menu, _, err := procCreatePopupMenu.Call()
	if menu == 0 {
		return err
	}
	t.menu = windows.Handle(menu)
	if err := appendMenuString(t.menu, trayMenuOpen, "打开浏览器"); err != nil {
		return err
	}
	if err := appendMenuSeparator(t.menu); err != nil {
		return err
	}
	if err := appendMenuString(t.menu, trayMenuExit, "退出"); err != nil {
		return err
	}
	return nil
}

func (t *trayController) destroyMenu() {
	if t.menu != 0 {
		procDestroyMenu.Call(uintptr(t.menu))
	}
}

func (t *trayController) showMenu() error {
	var p point
	res, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if res == 0 {
		return err
	}
	procSetForegroundWindow.Call(uintptr(t.hwnd))
	res, _, err = procTrackPopupMenu.Call(
		uintptr(t.menu),
		uintptr(tpmRightBtn|tpmBottom),
		uintptr(p.X),
		uintptr(p.Y),
		0,
		uintptr(t.hwnd),
		0,
	)
	if res == 0 {
		return err
	}
	procPostMessageW.Call(uintptr(t.hwnd), wmNull, 0, 0)
	return nil
}

func (t *trayController) openBrowser() {
	if err := openBrowserURL(t.browserURL); err != nil {
		log.Printf("open browser failed: %v", err)
	}
}

func (t *trayController) setTip(nid *notifyIconData, tip string) {
	b, _ := windows.UTF16FromString(tip)
	copy(nid.Tip[:], b)
}

func (t *trayController) quit() {
	t.cleanup.Do(func() {
		if t.onExit != nil {
			t.onExit()
		}
	})
	procPostQuitMessage.Call(0)
}

var trayWindow *trayController

func trayWndProc(hWnd windows.Handle, message uint32, wParam, lParam uintptr) uintptr {
	if trayWindow == nil {
		res, _, _ := procDefWindowProcW.Call(uintptr(hWnd), uintptr(message), wParam, lParam)
		return res
	}
	switch message {
	case trayMessageID:
		switch lParam {
		case wmLButtonUp:
			trayWindow.openBrowser()
		case wmRButtonUp:
			_ = trayWindow.showMenu()
		}
	case wmCommand:
		switch uint16(wParam & 0xffff) {
		case trayMenuOpen:
			trayWindow.openBrowser()
		case trayMenuExit:
			trayWindow.quit()
		}
	case wmClose:
		trayWindow.quit()
	case wmDestroy:
		trayWindow.quit()
	default:
		res, _, _ := procDefWindowProcW.Call(uintptr(hWnd), uintptr(message), wParam, lParam)
		return res
	}
	return 0
}

func appendMenuString(menu windows.Handle, id uint32, title string) error {
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	res, _, callErr := procAppendMenuW.Call(
		uintptr(menu),
		uintptr(mfString),
		uintptr(id),
		uintptr(unsafe.Pointer(titlePtr)),
	)
	if res == 0 {
		return callErr
	}
	return nil
}

func appendMenuSeparator(menu windows.Handle) error {
	res, _, callErr := procAppendMenuW.Call(
		uintptr(menu),
		uintptr(mfSeparator),
		0,
		0,
	)
	if res == 0 {
		return callErr
	}
	return nil
}

func runtimeLockThread() {
	runtime.LockOSThread()
}

func runtimeUnlockThread() {
	runtime.UnlockOSThread()
}

func openBrowserURL(url string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	result, _, callErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0,
		0,
		uintptr(windows.SW_SHOWNORMAL),
	)
	if result <= 32 {
		if callErr == syscall.Errno(0) {
			return fmt.Errorf("ShellExecuteW returned %d", result)
		}
		return callErr
	}
	return nil
}

func waitForV7UI(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout: 750 * time.Millisecond,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	defer client.CloseIdleConnections()

	var lastErr error
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + server.DefaultAddr + "/?v=7")
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && strings.Contains(string(body), "v1.0.0") {
				return nil
			}
			if readErr != nil {
				lastErr = readErr
			} else {
				lastErr = fmt.Errorf("localhost returned status %d without the v7 marker", response.StatusCode)
			}
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("localhost did not respond")
	}
	return lastErr
}
