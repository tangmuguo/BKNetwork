package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type enteredSnapshotContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *enteredSnapshotContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func waitSnapshotTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for snapshot collection")
	}
}

func TestSnapshotCollectionSharesOnlyConcurrentRequests(t *testing.T) {
	var group snapshotCollection
	var calls atomic.Int32
	release := make(chan struct{})
	collect := func() (networkSnapshot, error) {
		calls.Add(1)
		<-release
		return networkSnapshot{CollectedAt: "shared"}, nil
	}
	const clients = 20
	results := make(chan networkSnapshot, clients)
	for i := 0; i < clients; i++ {
		ctx := &enteredSnapshotContext{Context: context.Background(), entered: make(chan struct{})}
		go func() {
			snapshot, _ := group.collect(ctx, collect)
			results <- snapshot
		}()
		waitSnapshotTestSignal(t, ctx.entered)
	}
	close(release)
	for i := 0; i < clients; i++ {
		select {
		case snapshot := <-results:
			if snapshot.CollectedAt != "shared" {
				t.Fatalf("unexpected snapshot: %+v", snapshot)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("snapshot client did not finish")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("20 concurrent refreshes ran %d collections; want 1", got)
	}
	_, _ = group.collect(context.Background(), collect)
	if got := calls.Load(); got != 2 {
		t.Fatalf("completed snapshot was cached: calls=%d", got)
	}
}

func TestSnapshotCollectionCancellationDoesNotCancelOtherClients(t *testing.T) {
	var group snapshotCollection
	started, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	collect := func() (networkSnapshot, error) {
		close(started)
		<-release
		return networkSnapshot{CollectedAt: "available"}, nil
	}
	go func() { defer close(completed); _, _ = group.collect(context.Background(), collect) }()
	waitSnapshotTestSignal(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	observed := &enteredSnapshotContext{Context: ctx, entered: make(chan struct{})}
	result := make(chan error, 1)
	go func() { _, err := group.collect(observed, collect); result <- err }()
	waitSnapshotTestSignal(t, observed.entered)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled client kept waiting")
	}
	close(release)
	waitSnapshotTestSignal(t, completed)
	_, err := group.collect(ctx, func() (networkSnapshot, error) {
		t.Error("pre-canceled request collected")
		return networkSnapshot{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled request: %v", err)
	}
}

func TestSnapshotCollectionInvalidationSeparatesNetworkChanges(t *testing.T) {
	var group snapshotCollection
	startedOld, releaseOld, doneOld := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(doneOld)
		_, _ = group.collect(context.Background(), func() (networkSnapshot, error) {
			close(startedOld)
			<-releaseOld
			return networkSnapshot{CollectedAt: "old"}, nil
		})
	}()
	waitSnapshotTestSignal(t, startedOld)
	group.invalidate()
	startedNew, releaseNew, doneNew := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(doneNew)
		_, _ = group.collect(context.Background(), func() (networkSnapshot, error) {
			close(startedNew)
			<-releaseNew
			return networkSnapshot{CollectedAt: "new"}, nil
		})
	}()
	waitSnapshotTestSignal(t, startedNew)
	close(releaseOld)
	waitSnapshotTestSignal(t, doneOld)
	ctx := &enteredSnapshotContext{Context: context.Background(), entered: make(chan struct{})}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		snapshot, _ := group.collect(ctx, func() (networkSnapshot, error) {
			t.Error("old completion cleared the new in-flight collection")
			return networkSnapshot{}, nil
		})
		if snapshot.CollectedAt != "new" {
			t.Errorf("post-change client got %+v", snapshot)
		}
	}()
	waitSnapshotTestSignal(t, ctx.entered)
	close(releaseNew)
	waitSnapshotTestSignal(t, doneNew)
	waitSnapshotTestSignal(t, joined)
}

func TestSnapshotCollectionDoesNotCacheFailures(t *testing.T) {
	var group snapshotCollection
	want := errors.New("collection failed")
	_, err := group.collect(context.Background(), func() (networkSnapshot, error) { return networkSnapshot{}, want })
	if !errors.Is(err, want) {
		t.Fatalf("failure lost: %v", err)
	}
	snapshot, err := group.collect(context.Background(), func() (networkSnapshot, error) { return networkSnapshot{Online: true}, nil })
	if err != nil || !snapshot.Online {
		t.Fatalf("failure reused: %+v, %v", snapshot, err)
	}
}

func TestNetworkChangeHandlerInvalidatesBeforeAndAfterFailure(t *testing.T) {
	networkSnapshots.invalidate()
	t.Cleanup(networkSnapshots.invalidate)
	old := &snapshotCall{}
	networkSnapshots.current = old
	handler := NetworkChangeHandler(func(w http.ResponseWriter, r *http.Request) {
		if networkSnapshots.current != nil {
			t.Error("pre-change collection still joinable")
		}
		networkSnapshots.current = &snapshotCall{}
		w.WriteHeader(http.StatusInternalServerError)
	})
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/switch", nil))
	if networkSnapshots.current != nil {
		t.Fatal("collection during failed operation still joinable")
	}
	networkSnapshots.current = old
	NetworkChangeHandler(func(http.ResponseWriter, *http.Request) {}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/home-network", nil))
	if networkSnapshots.current != old {
		t.Fatal("read-only request invalidated collection")
	}
}
