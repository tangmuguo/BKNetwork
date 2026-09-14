package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"bknetwork/internal/appinfo"
	"bknetwork/internal/events"
	"bknetwork/internal/linuxnet"
	"bknetwork/internal/settings"
	"github.com/gorilla/websocket"
)

type networkManager interface {
	Status(context.Context) linuxnet.Status
	Connect(context.Context, string, string, string) error
	Disconnect(context.Context) error
}

type API struct {
	manager          networkManager
	settings         *settings.Store
	privileged       func() bool
	setAutoStart     func(context.Context, bool) error
	autoStartEnabled func(context.Context) bool
	operation        chan struct{}
}

var defaultMu sync.Mutex
var defaultAPI *API

func Configure(manager *linuxnet.Manager, store *settings.Store) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultAPI = newAPI(manager, store)
}

func newAPI(manager networkManager, store *settings.Store) *API {
	return &API{manager: manager, settings: store, privileged: func() bool { return os.Geteuid() == 0 },
		setAutoStart: settings.SetAutoStart, autoStartEnabled: settings.AutoStartEnabled, operation: make(chan struct{}, 1)}
}

func RegisterRoutes(mux *http.ServeMux, hub *events.Hub) {
	defaultMu.Lock()
	if defaultAPI == nil {
		dir := settings.DefaultStateDir()
		defaultAPI = newAPI(linuxnet.NewManager(dir), settings.NewStore(dir))
	}
	a := defaultAPI
	defaultMu.Unlock()
	a.register(mux, hub)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func apiError(w http.ResponseWriter, code int, err string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err})
}

func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	apiError(w, http.StatusMethodNotAllowed, "请求方法不支持")
	return false
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		apiError(w, 400, "请求 JSON 无效")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		apiError(w, 400, "请求中包含多余数据")
		return false
	}
	return true
}

func (a *API) mutate(w http.ResponseWriter, hub *events.Hub, name string, fn func(context.Context) error) {
	if !a.privileged() {
		apiError(w, 403, "当前为只读预览；请用 sudo ./bknetwork run --no-browser 启动以控制网络")
		return
	}
	select {
	case a.operation <- struct{}{}:
		defer func() { <-a.operation }()
	default:
		apiError(w, 409, "已有操作正在执行，请等待完成")
		return
	}
	hub.Publish(events.Event{Type: name + ".started", Message: "正在执行网络操作"})
	// Keep a user's connection request alive if its browser tab closes; every
	// operation has a deadline and the manager owns rollback on failure.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		hub.Publish(events.Event{Type: name + ".error", Message: err.Error()})
		apiError(w, 409, err.Error())
		return
	}
	hub.Publish(events.Event{Type: name + ".ok", Message: "操作完成，网络状态已更新"})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) register(mux *http.ServeMux, hub *events.Hub) {
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "GET") {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		cfg, err := a.settings.Load()
		if err != nil {
			apiError(w, 500, err.Error())
			return
		}
		cfg.AutoStart = a.autoStartEnabled(ctx)
		writeJSON(w, 200, map[string]any{"ok": true, "app": appinfo.Name, "version": appinfo.Version, "network": a.manager.Status(ctx), "settings": cfg})
	})
	mux.HandleFunc("/api/v1/home-network", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			defer cancel()
			s := a.manager.Status(ctx)
			writeJSON(w, 200, map[string]any{"ok": true, "profiles": s.Profiles, "profilesError": s.ProfilesError, "tunnelName": s.Profile, "status": s.WireGuard})
			return
		}
		if !method(w, r, "POST") {
			return
		}
		var p struct {
			IfName     string `json:"ifName"`
			TunnelName string `json:"tunnelName"`
			Action     string `json:"action"`
		}
		if !decode(w, r, &p) {
			return
		}
		if p.Action != "start" && p.Action != "stop" {
			apiError(w, 400, "未知 WireGuard 操作")
			return
		}
		a.mutate(w, hub, "wireguard", func(ctx context.Context) error {
			if p.Action == "stop" {
				return a.manager.Disconnect(ctx)
			}
			return a.manager.Connect(ctx, "wireguard", p.IfName, p.TunnelName)
		})
	})
	mux.HandleFunc("/api/v1/warp-mode", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "POST") {
			return
		}
		var p struct {
			IfName  string `json:"ifName"`
			Enabled *bool  `json:"enabled"`
		}
		if !decode(w, r, &p) {
			return
		}
		if p.Enabled == nil {
			apiError(w, 400, "缺少 enabled 字段")
			return
		}
		a.mutate(w, hub, "warp", func(ctx context.Context) error {
			if !*p.Enabled {
				return a.manager.Disconnect(ctx)
			}
			return a.manager.Connect(ctx, "warp", p.IfName, "")
		})
	})
	mux.HandleFunc("/api/v1/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "POST") {
			return
		}
		var p struct{}
		if !decode(w, r, &p) {
			return
		}
		a.mutate(w, hub, "restore", a.manager.Disconnect)
	})
	mux.HandleFunc("/api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			cfg, err := a.settings.Load()
			if err != nil {
				apiError(w, 500, err.Error())
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			cfg.AutoStart = a.autoStartEnabled(ctx)
			writeJSON(w, 200, map[string]any{"ok": true, "settings": cfg})
			return
		}
		if !method(w, r, "POST") {
			return
		}
		var payload struct {
			AutoStart *bool `json:"autoStart"`
		}
		if !decode(w, r, &payload) {
			return
		}
		if payload.AutoStart == nil {
			apiError(w, 400, "缺少 autoStart 字段")
			return
		}
		cfg := settings.Settings{AutoStart: *payload.AutoStart}
		a.mutate(w, hub, "settings", func(ctx context.Context) error {
			oldEnabled := a.autoStartEnabled(ctx)
			if cfg.AutoStart != oldEnabled {
				if err := a.setAutoStart(ctx, cfg.AutoStart); err != nil {
					return err
				}
			}
			if err := a.settings.Save(cfg); err != nil {
				if cfg.AutoStart != oldEnabled {
					return errors.Join(err, a.setAutoStart(ctx, oldEnabled))
				}
				return err
			}
			return nil
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "GET") {
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(req *http.Request) bool {
			origin := req.Header.Get("Origin")
			return origin == "" || origin == "http://"+req.Host
		}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(1024)
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		ch := hub.Subscribe(16)
		defer hub.Unsubscribe(ch)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			var event events.Event
			select {
			case <-closed:
				return
			case <-r.Context().Done():
				return
			case event = <-ch:
			case <-ticker.C:
				event = events.Event{Type: "heartbeat", Time: time.Now()}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(event) != nil {
				return
			}
		}
	})
}
