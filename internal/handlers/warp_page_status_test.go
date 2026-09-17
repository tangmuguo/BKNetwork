package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeWarpPageStatus(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		err       error
		status    string
		connected bool
		wantError bool
	}{
		{name: "connected", out: `{"status":"Connected"}`, status: "Connected", connected: true},
		{name: "disconnected", out: `{"status":"Disconnected","reason":"Manual"}`, status: "Disconnected"},
		{name: "connecting", out: `{"status":"Connecting"}`, status: "Connecting"},
		{name: "checking", out: `{"status":"Checking"}`, status: "Checking"},
		{name: "configuring", out: `{"status":"Configuring"}`, status: "Configuring"},
		{name: "legacy connected", out: "Status update: Connected\nNetwork: Healthy\n", status: "Connected", connected: true},
		{name: "empty output", wantError: true},
		{name: "missing status", out: `{}`, wantError: true},
		{name: "whitespace status", out: `{"status":" "}`, wantError: true},
		{name: "invalid output", out: "daemon unavailable", wantError: true},
		{name: "failed command", err: errors.New("exit status 1"), wantError: true},
		{name: "failure with disconnected output", out: `{"status":"Disconnected"}`, err: errors.New("exit status 1"), wantError: true},
		{name: "failure with connected output", out: `{"status":"Connected"}`, err: errors.New("exit status 1"), wantError: true},
		{name: "timeout with output", out: `{"status":"Disconnected"}`, err: context.DeadlineExceeded, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := probeWarpPageStatus(context.Background(), func(ctx context.Context, name string, args ...string) (string, error) {
				if name != "warp-cli" || strings.Join(args, " ") != "--json status" {
					t.Fatalf("unexpected command: %s %v", name, args)
				}
				deadline, ok := ctx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 3900*time.Millisecond || remaining > 4*time.Second {
					t.Fatalf("page status deadline = %v (set=%v), want four seconds", remaining, ok)
				}
				return tc.out, tc.err
			})
			if (result.Error != "") != tc.wantError {
				t.Fatalf("probe error = %q, wantError=%v", result.Error, tc.wantError)
			}
			if !tc.wantError && (result.Status != tc.status || result.Connected != tc.connected) {
				t.Fatalf("probe = %+v, want status=%q connected=%v", result, tc.status, tc.connected)
			}
		})
	}
}

func TestProbeWarpPageStatusRejectsExpiredContextOutput(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result := probeWarpPageStatus(ctx, func(ctx context.Context, _ string, _ ...string) (string, error) {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected expired parent context, got %v", ctx.Err())
		}
		return `{"status":"Connected"}`, nil
	})
	if result.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("late command output must be rejected, got %+v", result)
	}
}

func TestWarpStatusHandler(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		err       error
		connected bool
		wantError bool
	}{
		{name: "valid connection", out: `{"status":"Connected"}`, connected: true},
		{name: "valid disconnection", out: `{"status":"Disconnected","reason":"Manual"}`},
		{name: "failed read with output", out: `{"status":"Disconnected"}`, err: errors.New("daemon query failed"), wantError: true},
		{name: "timeout", err: context.DeadlineExceeded, wantError: true},
		{name: "empty read", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := warpStatusHandler(func(ctx context.Context, _ string, _ ...string) (string, error) {
				called = true
				deadline, ok := ctx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 3900*time.Millisecond || remaining > 4*time.Second {
					t.Fatalf("handler deadline = %v (set=%v), want four seconds", remaining, ok)
				}
				return tc.out, tc.err
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/warp-status", nil))
			if !called || recorder.Code != http.StatusOK {
				t.Fatalf("called=%v status=%d", called, recorder.Code)
			}
			var result warpSnapshot
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if (result.Error != "") != tc.wantError {
				t.Fatalf("response = %+v, wantError=%v", result, tc.wantError)
			}
			if !tc.wantError && (result.Connected != tc.connected || result.Status == "") {
				t.Fatalf("response lost valid status: %+v", result)
			}
		})
	}
}

func TestWarpStatusHandlerHonorsRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := warpStatusHandler(func(ctx context.Context, _ string, _ ...string) (string, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("request cancellation not propagated: %v", ctx.Err())
		}
		return `{"status":"Disconnected"}`, nil
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/warp-status", nil).WithContext(ctx)
	handler.ServeHTTP(recorder, request)
	var result warpSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != context.Canceled.Error() {
		t.Fatalf("canceled read must carry an error: %+v", result)
	}
}

func TestWarpStatusHandlerRejectsNonGET(t *testing.T) {
	handler := warpStatusHandler(func(context.Context, string, ...string) (string, error) {
		t.Fatal("non-GET request must not invoke warp-cli")
		return "", nil
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/warp-status", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}
