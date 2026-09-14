package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bknetwork/internal/events"
	"bknetwork/internal/linuxnet"
	"bknetwork/internal/settings"
)

type fakeManager struct {
	calls []string
	err   error
	state linuxnet.Status
}

func (m *fakeManager) Status(context.Context) linuxnet.Status { return m.state }
func (m *fakeManager) Connect(_ context.Context, mode, iface, profile string) error {
	m.calls = append(m.calls, mode+":"+iface+":"+profile)
	return m.err
}
func (m *fakeManager) Disconnect(context.Context) error {
	m.calls = append(m.calls, "disconnect")
	return m.err
}

func testAPI(t *testing.T, privileged bool) (*API, *fakeManager, *http.ServeMux) {
	t.Helper()
	m := &fakeManager{}
	a := newAPI(m, settings.NewStore(t.TempDir()))
	a.privileged = func() bool { return privileged }
	a.autoStartEnabled = func(context.Context) bool { return false }
	a.setAutoStart = func(context.Context, bool) error { return nil }
	mux := http.NewServeMux()
	a.register(mux, events.NewHub())
	return a, m, mux
}

func request(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestPreviewCannotChangeNetworkOrSettings(t *testing.T) {
	_, m, mux := testAPI(t, false)
	for _, test := range []struct{ path, body string }{
		{"/api/v1/warp-mode", `{"ifName":"wlp1s0","enabled":true}`},
		{"/api/v1/home-network", `{"ifName":"wlp1s0","tunnelName":"home","action":"start"}`},
		{"/api/v1/disconnect", `{}`},
		{"/api/v1/settings", `{"autoStart":true}`},
	} {
		w := request(mux, "POST", test.path, test.body)
		if w.Code != 403 {
			t.Errorf("%s: status %d", test.path, w.Code)
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("read-only requests mutated networking: %v", m.calls)
	}
}

func TestInvalidRequestsCannotTriggerDisconnect(t *testing.T) {
	_, m, mux := testAPI(t, true)
	for _, body := range []string{`{}`, `{"enabled":"false"}`, `{"enabled":null}`, `{"enabled":false} {}`, `{"enabled":false,"unexpected":1}`, strings.Repeat(" ", 17000) + `{"enabled":false}`} {
		if w := request(mux, "POST", "/api/v1/warp-mode", body); w.Code != 400 {
			t.Errorf("expected 400, got %d", w.Code)
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("invalid requests mutated networking: %v", m.calls)
	}
}

func TestModesAndUniversalRecoveryAreRouted(t *testing.T) {
	_, m, mux := testAPI(t, true)
	for _, test := range []struct{ path, body, want string }{
		{"/api/v1/warp-mode", `{"ifName":"wlp1s0","enabled":true}`, "warp:wlp1s0:"},
		{"/api/v1/home-network", `{"ifName":"enp2s0","tunnelName":"home","action":"start"}`, "wireguard:enp2s0:home"},
		{"/api/v1/disconnect", `{}`, "disconnect"},
	} {
		if w := request(mux, "POST", test.path, test.body); w.Code != 200 {
			t.Fatalf("%s: %s", test.path, w.Body)
		}
		if m.calls[len(m.calls)-1] != test.want {
			t.Fatalf("calls=%v", m.calls)
		}
	}
}

func TestFailedRecoveryRemainsAnError(t *testing.T) {
	_, m, mux := testAPI(t, true)
	m.err = errors.New("原网络恢复失败，保留恢复记录")
	w := request(mux, "POST", "/api/v1/disconnect", `{}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), "保留恢复记录") {
		t.Fatalf("recovery failure was hidden: %d %s", w.Code, w.Body)
	}
}

func TestConcurrentMutationRejected(t *testing.T) {
	a, m, mux := testAPI(t, true)
	a.operation <- struct{}{}
	defer func() { <-a.operation }()
	w := request(mux, "POST", "/api/v1/disconnect", `{}`)
	if w.Code != 409 || len(m.calls) != 0 {
		t.Fatalf("busy mutation accepted: %d %v", w.Code, m.calls)
	}
}

func TestAutostartFailureDoesNotPersistSettings(t *testing.T) {
	a, _, mux := testAPI(t, true)
	a.setAutoStart = func(context.Context, bool) error { return errors.New("service not installed") }
	w := request(mux, "POST", "/api/v1/settings", `{"autoStart":true}`)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	cfg, err := a.settings.Load()
	if err != nil || cfg.AutoStart {
		t.Fatalf("failed settings saved: %+v %v", cfg, err)
	}
}
