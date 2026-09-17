package handlers

import (
	"context"
	"net/http"
	"sync"
)

// A collection is shared only while it is running. Completed snapshots are
// never cached, so the next refresh still reads the current network state.
type snapshotCollection struct {
	mu      sync.Mutex
	current *snapshotCall
}

type snapshotCall struct {
	done     chan struct{}
	snapshot networkSnapshot
	err      error
}

var networkSnapshots snapshotCollection

func (g *snapshotCollection) collect(ctx context.Context, collect func() (networkSnapshot, error)) (networkSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return networkSnapshot{}, err
	}
	g.mu.Lock()
	call := g.current
	if call == nil {
		call = &snapshotCall{done: make(chan struct{})}
		g.current = call
		go func() {
			call.snapshot, call.err = collect()
			g.mu.Lock()
			if g.current == call {
				g.current = nil
			}
			close(call.done)
			g.mu.Unlock()
		}()
	}
	g.mu.Unlock()
	select {
	case <-ctx.Done():
		return networkSnapshot{}, ctx.Err()
	case <-call.done:
		return call.snapshot, call.err
	}
}

func (g *snapshotCollection) invalidate() {
	g.mu.Lock()
	g.current = nil
	g.mu.Unlock()
}

func collectNetworkSnapshotContext(ctx context.Context) (networkSnapshot, error) {
	return networkSnapshots.collect(ctx, collectNetworkSnapshotUnshared)
}

// NetworkChangeHandler separates collections across a user operation, including
// failed operations that may have partially changed or restored the network.
func NetworkChangeHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			networkSnapshots.invalidate()
			defer networkSnapshots.invalidate()
		}
		next(w, r)
	}
}
