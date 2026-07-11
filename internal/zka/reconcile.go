package zka

import (
	"context"
	"time"
)

func (d *Daemon) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.reconcile(ctx)
		}
	}
}

func (d *Daemon) reconcile(ctx context.Context) {
	d.mu.Lock()
	endpoints := map[string]bool{}
	for _, session := range d.state.Sessions {
		for _, view := range session.Views {
			if view.Endpoint != "" {
				endpoints[view.Endpoint] = true
			}
		}
	}
	d.mu.Unlock()
	results := make(map[string]reconcileResult, len(endpoints))
	for endpoint := range endpoints {
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		tree, err := d.kitty.List(queryCtx, endpoint)
		cancel()
		results[endpoint] = reconcileResult{tree: tree, reachable: err == nil}
	}
	d.applyReconcile(results)
}

type reconcileResult struct {
	tree      []kittyOSWindow
	reachable bool
}

func (d *Daemon) applyReconcile(results map[string]reconcileResult) {
	now := time.Now().UTC()
	var seen []*Session
	d.mu.Lock()
	changed := false
	for endpoint, result := range results {
		found := findManagedViews(result.tree)
		for _, session := range d.state.Sessions {
			expected := map[string]View{}
			if result.reachable {
				for _, view := range found[session.ID] {
					view.Endpoint = endpoint
					view.LastSeen = now
					expected[view.Key()] = view
				}
			}
			for key, view := range session.Views {
				if view.Endpoint != endpoint {
					continue
				}
				next, exists := expected[key]
				if exists {
					delete(expected, key)
				} else {
					next = view
					next.Attached = false
					next.Focused = false
				}
				if view.Attached != next.Attached || view.Focused != next.Focused {
					changed = true
				}
				session.Views[key] = next
			}
			for key, view := range expected {
				session.Views[key] = view
				changed = true
			}
		}
	}
	for _, session := range d.state.Sessions {
		if session.State == StateDone && session.AnyFocused() {
			session.State = StateIdle
			session.Evidence = Evidence{Source: "kitty", Event: "focused", Timestamp: now}
			session.UpdatedAt = now
			seen = append(seen, session.Clone())
			changed = true
		}
	}
	if changed {
		_ = d.store.Save(d.state)
	}
	d.mu.Unlock()
	for _, session := range seen {
		go d.closeDesktopNotifications(context.Background(), session)
		go d.updateKittyState(context.Background(), session)
	}
}
