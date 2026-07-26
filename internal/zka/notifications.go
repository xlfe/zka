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

func notificationBody(workspace *Workspace, pane *Pane, includeEvidence bool) string {
	detail := "State: " + string(pane.State)
	if includeEvidence {
		detail = pane.Evidence.Detail
		if detail == "" {
			detail = pane.Evidence.Event
		}
	}
	reference := workspace.ID
	if workspace.RemoteHost != "" {
		reference = workspace.RemoteHost + ":" + workspace.ID
	}
	return fmt.Sprintf("%s\nOrigin: %s\nWorkspace: %s\nPane: %s\nOpen: zka workspace attach %s --pane %s",
		detail, workspace.Origin.Name, workspace.ID, pane.ID, reference, pane.ID)
}

func (d *Daemon) afterTransition(ctx context.Context, before AgentState, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	if d.attentionStateEnabled(pane.State) {
		d.reconcile(ctx)
		if fresh, err := d.getWorkspace(workspace.ID); err == nil {
			workspace = fresh
			pane = fresh.Panes[paneID]
		}
		if pane == nil || !d.attentionStateEnabled(pane.State) {
			return
		}
	}
	d.updateKittyState(ctx, workspace, paneID)
	_, focused := attentionPaneView(workspace, paneID)
	if !d.attentionStateEnabled(pane.State) || (pane.State == StateDone && focused) {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if d.config.Notifications.DesktopEnabled {
		if attachment, view := d.firstUnfocusedView(workspace, paneID); attachment != nil {
			d.sendDesktop(ctx, attachment, view, workspace, pane)
		}
	}
	important := pane.State == StateBlocked || pane.State == StateError || (pane.State == StateDone && !paneAttached(workspace, paneID))
	if important && d.config.Notifications.NtfyEnabled {
		d.sendNtfy(ctx, workspace, pane)
	}
	_ = before
}

func (d *Daemon) afterRemoteTransition(ctx context.Context, workspace *Workspace, paneID string) {
	d.updateKittyState(ctx, workspace, paneID)
	d.afterRemoteTransitionNotification(ctx, workspace, paneID)
}

func (d *Daemon) afterRemoteTransitionNotification(ctx context.Context, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	_, focused := attentionPaneView(workspace, paneID)
	if !d.attentionStateEnabled(pane.State) || (pane.State == StateDone && focused) {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if d.config.Notifications.DesktopEnabled {
		if attachment, view := d.firstUnfocusedView(workspace, paneID); attachment != nil {
			d.sendDesktop(ctx, attachment, view, workspace, pane)
		}
	}
}

// resumeAttentionNotifications projects the current actionable set back onto
// delivery channels. Notification records de-duplicate anything delivered
// before focus mode, while items that resolved during focus mode are absent.
func (d *Daemon) resumeAttentionNotifications(ctx context.Context) {
	snapshot := d.attentionSnapshot()
	if snapshot.Paused {
		return
	}
	for _, item := range snapshot.Items {
		d.mu.Lock()
		workspace := d.state.Workspaces[item.WorkspaceID]
		if workspace != nil {
			workspace = workspace.Clone()
		}
		d.mu.Unlock()
		if workspace == nil {
			continue
		}
		pane := workspace.Panes[item.PaneID]
		if pane == nil {
			continue
		}
		if d.config.Notifications.DesktopEnabled {
			if attachment, view := d.firstUnfocusedView(workspace, pane.ID); attachment != nil {
				d.sendDesktop(ctx, attachment, view, workspace, pane)
			}
		}
		important := pane.State == StateBlocked || pane.State == StateError || (pane.State == StateDone && !paneAttached(workspace, pane.ID))
		if workspace.RemoteHost == "" && important && d.config.Notifications.NtfyEnabled {
			d.sendNtfy(ctx, workspace, pane)
		}
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

func isLocalUnixAttachment(attachment *Attachment, nodeID string) bool {
	return attachment != nil &&
		attachment.Node.ID == nodeID &&
		strings.HasPrefix(attachment.Endpoint, "unix:")
}

func isReadyLocalKittyAttachment(attachment *Attachment, nodeID string) bool {
	return isLocalUnixAttachment(attachment, nodeID) &&
		attachment.Status == AttachmentReady &&
		!attachment.Revoked
}

func (d *Daemon) localNodeID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.Node.ID
}

func (d *Daemon) firstUnfocusedView(workspace *Workspace, paneID string) (*Attachment, RuntimeView) {
	localNodeID := d.localNodeID()
	for _, attachment := range workspace.SortedAttachments() {
		if !isReadyLocalKittyAttachment(attachment, localNodeID) {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready && !view.Focused {
			return attachment, view
		}
	}
	return nil, RuntimeView{}
}

func (d *Daemon) updateKittyState(ctx context.Context, workspace *Workspace, paneIDs ...string) {
	localNodeID := d.localNodeID()
	var selected map[string]bool
	if len(paneIDs) != 0 {
		selected = make(map[string]bool, len(paneIDs))
		for _, paneID := range paneIDs {
			selected[paneID] = true
		}
	}
	for _, attachment := range workspace.SortedAttachments() {
		if !isReadyLocalKittyAttachment(attachment, localNodeID) {
			continue
		}
		updated := false
		for paneID, view := range attachment.Views {
			if selected != nil && !selected[paneID] {
				continue
			}
			pane := workspace.Panes[paneID]
			if pane == nil || !view.Ready {
				continue
			}
			updated = true
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.SetPaneState(callCtx, attachment.Endpoint, view, workspace, pane)
			cancel()
			if err != nil {
				d.logger.Printf("update kitty state workspace=%s pane=%s: %v", workspace.ID, paneID, err)
			}
		}
		if updated {
			d.applyTabTitles(ctx, attachment.Endpoint, workspace)
		}
	}
}

// desiredTabName is the single formula for a managed tab's Kitty name: the
// name the desired topology owns, decorated with the worst state among its
// panes. An empty name means the tab must stay unnamed so Kitty falls back to
// its active window's title.
//
// Having one formula matters. Previously the reconciler wrote the bare title on
// every pass while this path wrote a decorated one, so the two overwrote each
// other, the tab bar flickered, and the captured value oscillated.
func desiredTabName(workspace *Workspace, tabNode Node) string {
	if tabNode.Title == "" {
		return ""
	}
	highest := StateIdle
	for _, paneNode := range tabNode.Children {
		if pane := workspace.Panes[paneNode.PaneID]; pane != nil {
			if statePriority(pane.State) > statePriority(highest) {
				highest = pane.State
			}
		}
	}
	return strings.TrimSpace(stateMarker(highest) + " " + tabNode.Title)
}

// applyTabTitles is the only caller of SetTabTitle. It writes only where the
// live name already differs, so a settled workspace issues no calls at all.
func (d *Daemon) applyTabTitles(ctx context.Context, endpoint string, workspace *Workspace) {
	if len(workspace.Topology.Roots) == 0 {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tree, err := d.kitty.List(callCtx, endpoint)
	cancel()
	if err != nil {
		return
	}
	liveTabs := map[string]kittyTab{}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] == workspace.ID {
					liveTabs[window.UserVars["zka_pane"]] = tab
				}
			}
		}
	}
	seen := map[int64]bool{}
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			tab, ok := liveTabs[tabNode.Children[0].PaneID]
			if !ok || seen[tab.ID] {
				continue
			}
			seen[tab.ID] = true
			want := desiredTabName(workspace, tabNode)
			if canonicalStrippedValue(tab.Title) == canonicalStrippedValue(want) {
				continue
			}
			if want == "" && tab.namedTitle() == "" {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.SetTabTitle(callCtx, endpoint, tab.ID, want)
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
			if err := d.kitty.FocusPane(focusCtx, attachment.Endpoint, workspace.ID, pane.ID); err != nil {
				d.logger.Printf("focus Kitty pane from notification: %v", err)
			}
			if err := focusSwayWindow(focusCtx, d.runner, attachment.PID); err != nil {
				d.logger.Printf("focus Sway window from notification: %v", err)
			}
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
	localNodeID := d.localNodeID()
	for _, attachment := range workspace.Attachments {
		if !isLocalUnixAttachment(attachment, localNodeID) ||
			attachment.Status == AttachmentDetached ||
			attachment.Revoked {
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
	title, body := notificationTitle(workspace, pane), notificationBody(workspace, pane, d.config.Notifications.NtfyIncludeEvidence)
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
	if d.state.AttentionPaused {
		return false
	}
	if channel == "kitty" && !d.config.Notifications.DesktopEnabled {
		return false
	}
	if channel == "ntfy" && !d.config.Notifications.NtfyEnabled {
		return false
	}
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
