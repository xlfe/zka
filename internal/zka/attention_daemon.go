package zka

import (
	"context"
	"encoding/json"
	"net"
	"time"
)

const attentionHeartbeatInterval = 30 * time.Second

func (d *Daemon) attentionStateEnabled(state AgentState) bool {
	for _, enabled := range d.config.Attention.States {
		if state == enabled {
			return true
		}
	}
	return false
}

func (d *Daemon) attentionSnapshot() AttentionSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return buildAttentionSnapshot(d.state, d.config.Attention.States)
}

func (d *Daemon) setAttentionPaused(paused bool) (AttentionSnapshot, error) {
	return d.changeAttentionPaused(func(bool) bool { return paused })
}

func (d *Daemon) toggleAttentionPaused() (AttentionSnapshot, error) {
	return d.changeAttentionPaused(func(paused bool) bool { return !paused })
}

func (d *Daemon) changeAttentionPaused(resolve func(bool) bool) (AttentionSnapshot, error) {
	d.mu.Lock()
	previous := d.state.AttentionPaused
	paused := resolve(previous)
	d.state.AttentionPaused = paused
	if previous != paused {
		if err := d.store.Save(d.state); err != nil {
			d.state.AttentionPaused = previous
			d.mu.Unlock()
			return AttentionSnapshot{}, err
		}
	}
	snapshot := buildAttentionSnapshot(d.state, d.config.Attention.States)
	workspaces := make([]*Workspace, 0, len(d.state.Workspaces))
	if previous != paused && paused {
		for _, workspace := range d.state.Workspaces {
			workspaces = append(workspaces, workspace.Clone())
		}
	}
	d.mu.Unlock()

	if previous != paused && paused {
		d.startWorker(func(ctx context.Context) {
			for _, workspace := range workspaces {
				d.closeDesktopNotifications(ctx, workspace, "")
			}
		})
	}
	if previous != paused && !paused {
		d.startWorker(func(ctx context.Context) { d.resumeAttentionNotifications(ctx) })
	}
	return snapshot, nil
}

func (d *Daemon) signalAttention() {
	d.attentionMu.Lock()
	defer d.attentionMu.Unlock()
	for subscriber := range d.attentionSubs {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (d *Daemon) subscribeAttention() (<-chan struct{}, func()) {
	updates := make(chan struct{}, 1)
	d.attentionMu.Lock()
	d.attentionSubs[updates] = struct{}{}
	d.attentionMu.Unlock()
	return updates, func() {
		d.attentionMu.Lock()
		delete(d.attentionSubs, updates)
		d.attentionMu.Unlock()
	}
}

func (d *Daemon) watchAttention(ctx context.Context, conn net.Conn) {
	updates, unsubscribe := d.subscribeAttention()
	defer unsubscribe()
	ticker := time.NewTicker(attentionHeartbeatInterval)
	defer ticker.Stop()
	var fingerprint string
	send := func(force bool) error {
		snapshot := d.attentionSnapshot()
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if !force && string(raw) == fingerprint {
			return nil
		}
		fingerprint = string(raw)
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return json.NewEncoder(conn).Encode(response{Version: protocolVersion, OK: true, Data: raw})
	}
	if err := send(true); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-updates:
			if err := send(false); err != nil {
				return
			}
		case <-ticker.C:
			if err := send(true); err != nil {
				return
			}
		}
	}
}
