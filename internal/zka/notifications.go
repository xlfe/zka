package zka

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func notificationTitle(workspace *Workspace, pane *Pane) string {
	switch pane.State {
	case StateBlocked:
		return "zka: " + workspace.Name + " needs input"
	case StateError:
		return "zka: " + workspace.Name + " failed"
	case StateDone:
		return "zka: " + workspace.Name + " finished"
	default:
		return "zka: " + workspace.Name
	}
}

func notificationBody(workspace *Workspace, pane *Pane) string {
	detail := pane.Evidence.Detail
	if detail == "" {
		detail = pane.Evidence.Event
	}
	return fmt.Sprintf("%s\nOrigin: %s\nWorkspace: %s\nPane: %s\nFocus: zka workspace focus %s --pane %s",
		detail, workspace.Origin.Name, workspace.ID, pane.ID, workspace.ID, pane.ID)
}

func (d *Daemon) afterTransition(ctx context.Context, before AgentState, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	if pane.State == StateBlocked || pane.State == StateError || pane.State == StateDone {
		d.reconcile(ctx)
		if fresh, err := d.getWorkspace(workspace.ID); err == nil {
			workspace = fresh
			pane = fresh.Panes[paneID]
		}
		if pane == nil || (pane.State != StateBlocked && pane.State != StateError && pane.State != StateDone) {
			return
		}
	}
	d.updateKittyState(ctx, workspace)
	if pane.State != StateBlocked && pane.State != StateError && pane.State != StateDone {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if attachment, view := firstUnfocusedView(workspace, paneID); attachment != nil {
		d.sendDesktop(ctx, attachment, view, workspace, pane)
	}
	important := pane.State == StateBlocked || pane.State == StateError || (pane.State == StateDone && !paneAttached(workspace, paneID))
	if important {
		d.sendNtfy(ctx, workspace, pane)
	}
	_ = before
}

func (d *Daemon) afterRemoteTransition(ctx context.Context, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	d.updateKittyState(ctx, workspace)
	if pane.State != StateBlocked && pane.State != StateError && pane.State != StateDone {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if attachment, view := firstUnfocusedView(workspace, paneID); attachment != nil {
		d.sendDesktop(ctx, attachment, view, workspace, pane)
	}
}

func paneAttached(workspace *Workspace, paneID string) bool {
	for _, attachment := range workspace.Attachments {
		if attachment.Status != AttachmentReady {
			continue
		}
		if attachment.Transport.Kind == "ssh" && !clientHeartbeatFresh(attachment.ClientHeartbeats[paneID], time.Now().UTC()) {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready {
			return true
		}
	}
	return false
}

func firstUnfocusedView(workspace *Workspace, paneID string) (*Attachment, RuntimeView) {
	for _, attachment := range workspace.SortedAttachments() {
		if !strings.HasPrefix(attachment.Endpoint, "unix:") || attachment.Status == AttachmentDetached {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready && !view.Focused {
			return attachment, view
		}
	}
	return nil, RuntimeView{}
}

func (d *Daemon) updateKittyState(ctx context.Context, workspace *Workspace) {
	for _, attachment := range workspace.SortedAttachments() {
		if !strings.HasPrefix(attachment.Endpoint, "unix:") || attachment.Status == AttachmentDetached {
			continue
		}
		for paneID, view := range attachment.Views {
			pane := workspace.Panes[paneID]
			if pane == nil || !view.Ready {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.SetPaneState(callCtx, attachment.Endpoint, view, workspace, pane)
			cancel()
			if err != nil {
				d.logger.Printf("update kitty state workspace=%s pane=%s: %v", workspace.ID, paneID, err)
			}
		}
		d.updateKittyTabTitles(ctx, attachment.Endpoint, workspace)
	}
}

func (d *Daemon) updateKittyTabTitles(ctx context.Context, endpoint string, workspace *Workspace) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tree, err := d.kitty.List(callCtx, endpoint)
	cancel()
	if err != nil {
		return
	}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			highest := StateIdle
			hasManaged := false
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] != workspace.ID {
					continue
				}
				pane := workspace.Panes[window.UserVars["zka_pane"]]
				if pane == nil {
					continue
				}
				hasManaged = true
				if statePriority(pane.State) > statePriority(highest) {
					highest = pane.State
				}
			}
			if !hasManaged {
				continue
			}
			title := strings.TrimSpace(stateMarker(highest) + " " + stripStateMarker(tab.Title))
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.SetTabTitle(callCtx, endpoint, tab.ID, title)
			cancel()
		}
	}
}

func statePriority(state AgentState) int {
	switch state {
	case StateError:
		return 5
	case StateBlocked:
		return 4
	case StateDone:
		return 3
	case StateWorking:
		return 2
	case StateUnknown:
		return 1
	default:
		return 0
	}
}

func stripStateMarker(title string) string {
	for _, state := range []AgentState{StateError, StateBlocked, StateDone, StateWorking, StateUnknown} {
		prefix := stateMarker(state) + " "
		if strings.HasPrefix(title, prefix) {
			return strings.TrimPrefix(title, prefix)
		}
	}
	return title
}

func (d *Daemon) sendDesktop(ctx context.Context, attachment *Attachment, view RuntimeView, workspace *Workspace, pane *Pane) {
	key := "kitty:" + string(pane.State) + ":" + eventIdentity(pane)
	if !d.reserveNotification(workspace.ID, pane.ID, key, "kitty") {
		return
	}
	d.startWorker(func(workerCtx context.Context) {
		choice, err := d.kitty.Notify(workerCtx, view, attachment.Endpoint, workspace, pane)
		if err != nil {
			d.finishNotification(workspace.ID, pane.ID, key, err)
			return
		}
		d.finishNotification(workspace.ID, pane.ID, key, nil)
		choice = strings.TrimSpace(choice)
		if choice == "0" || choice == "1" {
			focusCtx, cancel := context.WithTimeout(workerCtx, 3*time.Second)
			_ = d.kitty.FocusPane(focusCtx, attachment.Endpoint, workspace.ID, pane.ID)
			if workspace.RemoteHost != "" {
				_, _ = d.remotes.Call(focusCtx, workspace.RemoteHost, "seen", workspacePaneRequest{Workspace: workspace.ID, Pane: pane.ID})
			} else {
				_, _ = d.markSeen(workspace.ID, pane.ID)
			}
			cancel()
		}
	})
	_ = ctx
}

func (d *Daemon) closeDesktopNotifications(ctx context.Context, workspace *Workspace, paneRef string) {
	for _, attachment := range workspace.Attachments {
		if !strings.HasPrefix(attachment.Endpoint, "unix:") {
			continue
		}
		for paneID := range workspace.Panes {
			if paneRef != "" && paneID != paneRef {
				continue
			}
			d.kitty.CloseNotification(ctx, attachment.Endpoint, workspace.ID, paneID)
		}
	}
}

func (d *Daemon) sendNtfy(ctx context.Context, workspace *Workspace, pane *Pane) {
	key := "ntfy:" + string(pane.State) + ":" + eventIdentity(pane)
	if !d.reserveNotification(workspace.ID, pane.ID, key, "ntfy") {
		return
	}
	priority, tag := "3", "white_check_mark"
	if pane.State == StateBlocked {
		priority, tag = "5", "warning"
	}
	if pane.State == StateError {
		priority, tag = "5", "rotating_light"
	}
	title, body := notificationTitle(workspace, pane), notificationBody(workspace, pane)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, lastErr = d.runner.Run(callCtx, d.config.Notifications.NtfyCommand, "-T", title, "-p", priority, "-g", tag, body)
		cancel()
		if lastErr == nil || ctx.Err() != nil {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				break
			case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
			}
		}
	}
	d.finishNotification(workspace.ID, pane.ID, key, lastErr)
	if lastErr != nil && ctx.Err() == nil {
		d.logger.Printf("ntfy delivery failed workspace=%s pane=%s: %v", workspace.ID, pane.ID, lastErr)
	}
}

func eventIdentity(pane *Pane) string {
	if pane.LastTurnID != "" {
		return pane.LastTurnID
	}
	return pane.Evidence.Timestamp.Format(time.RFC3339Nano)
}

func (d *Daemon) reserveNotification(workspaceID, paneID, key, channel string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return false
	}
	pane := workspace.Panes[paneID]
	if pane.Notifications == nil {
		pane.Notifications = map[string]NotificationRecord{}
	}
	if _, exists := pane.Notifications[key]; exists {
		return false
	}
	pane.Notifications[key] = NotificationRecord{Key: key, Channel: channel}
	_ = d.store.Save(d.state)
	return true
}

func (d *Daemon) finishNotification(workspaceID, paneID, key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return
	}
	pane := workspace.Panes[paneID]
	record := pane.Notifications[key]
	if err != nil {
		record.LastError = err.Error()
	} else {
		record.SentAt = time.Now().UTC()
		record.LastError = ""
	}
	pane.Notifications[key] = record
	_ = d.store.Save(d.state)
}
