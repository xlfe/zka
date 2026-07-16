package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

type debounceEntry struct {
	first time.Time
	due   time.Time
}

func (d *Daemon) watcherReadLoop(ctx context.Context) {
	buffer := make([]byte, 64<<10)
	for {
		n, _, err := d.watcher.ReadFromUnix(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			d.failWorker(fmt.Errorf("read kitty watcher event: %w", err))
			return
		}
		var event WatcherEvent
		if err := json.Unmarshal(buffer[:n], &event); err != nil {
			d.logger.Printf("drop invalid kitty watcher event: %v", err)
			continue
		}
		if event.Version != 1 || event.Endpoint == "" || event.Kind == "" {
			d.logger.Printf("drop incomplete kitty watcher event")
			continue
		}
		select {
		case d.events <- event:
		case <-ctx.Done():
			return
		default:
			d.logger.Printf("drop kitty watcher event for busy endpoint %s", event.Endpoint)
		}
	}
}

func (d *Daemon) topologyLoop(ctx context.Context) {
	check := time.NewTicker(50 * time.Millisecond)
	fallback := time.NewTicker(2 * time.Second)
	defer check.Stop()
	defer fallback.Stop()
	pending := map[string]debounceEntry{}
	closing := map[string]map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-d.events:
			if event.Kind == "quit" {
				if event.Confirmed && !event.Aborted {
					delete(pending, event.Endpoint)
					delete(closing, event.Endpoint)
					d.scheduleEndpointDeletion(event.Endpoint)
				}
				continue
			}
			if event.Kind == "close" && event.PaneID != "" {
				if closing[event.Endpoint] == nil {
					closing[event.Endpoint] = map[string]bool{}
				}
				closing[event.Endpoint][event.PaneID] = true
				if d.closeEventsCoverWorkspace(event.Endpoint, closing[event.Endpoint]) {
					delete(pending, event.Endpoint)
					delete(closing, event.Endpoint)
					d.scheduleEndpointDeletion(event.Endpoint)
					continue
				}
			}
			now := time.Now()
			entry, exists := pending[event.Endpoint]
			if !exists {
				entry.first = now
			}
			entry.due = now.Add(150 * time.Millisecond)
			if maximum := entry.first.Add(time.Second); entry.due.After(maximum) {
				entry.due = maximum
			}
			pending[event.Endpoint] = entry
		case now := <-check.C:
			for endpoint, entry := range pending {
				if now.Before(entry.due) {
					continue
				}
				delete(pending, endpoint)
				d.scheduleCapture(endpoint)
			}
		case <-fallback.C:
			for _, endpoint := range d.attachmentEndpoints() {
				if _, queued := pending[endpoint]; !queued {
					d.scheduleCapture(endpoint)
				}
			}
		}
	}
}

func (d *Daemon) scheduleCapture(endpoint string) {
	d.captureMu.Lock()
	if d.capturing[endpoint] {
		d.captureMu.Unlock()
		return
	}
	d.capturing[endpoint] = true
	d.captureMu.Unlock()
	d.startWorker(func(ctx context.Context) {
		defer func() { d.captureMu.Lock(); delete(d.capturing, endpoint); d.captureMu.Unlock() }()
		d.captureEndpoint(ctx, endpoint)
	})
}

func (d *Daemon) attachmentEndpoints() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[string]bool{}
	var endpoints []string
	for _, workspace := range d.state.Workspaces {
		if workspace.DeletionPending {
			continue
		}
		for _, attachment := range workspace.Attachments {
			if attachment.Endpoint == "" || attachment.Status == AttachmentDetached {
				continue
			}
			if attachment.Node.ID != d.state.Node.ID {
				continue
			}
			if !seen[attachment.Endpoint] {
				endpoints = append(endpoints, attachment.Endpoint)
				seen[attachment.Endpoint] = true
			}
		}
	}
	return endpoints
}

func (d *Daemon) endpointAttachment(endpoint string) (*Workspace, *Attachment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, workspace := range d.state.Workspaces {
		if workspace.DeletionPending {
			continue
		}
		for _, attachment := range workspace.Attachments {
			if attachment.Endpoint == endpoint && attachment.Status != AttachmentDetached {
				return workspace.Clone(), attachment.Clone()
			}
		}
	}
	return nil, nil
}

func (d *Daemon) captureEndpoint(ctx context.Context, endpoint string) {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil {
		return
	}
	if attachment.Revoked {
		if !attachment.RevocationClosed {
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.CloseWorkspace(callCtx, endpoint, workspace.ID)
			cancel()
			if err != nil {
				return
			}
			d.markRevocationClosed(workspace.ID, attachment.ID)
		}
		if workspace.RemoteHost != "" {
			remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := d.remotes.Call(remoteCtx, workspace.RemoteHost, "detach_attachment", attachmentRefRequest{Workspace: workspace.ID, Attachment: attachment.ID})
			cancel()
			if err != nil {
				return
			}
		}
		_, _ = d.detachAttachment(workspace.ID, attachment.ID)
		return
	}
	deadline := time.Now().Add(time.Second)
	var manifest Manifest
	var views map[string]RuntimeView
	var err error
	for {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		manifest, views, err = CaptureManifest(callCtx, d.kitty, endpoint, workspace)
		cancel()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		workspace, attachment = d.endpointAttachment(endpoint)
		if workspace == nil || attachment == nil {
			return
		}
	}
	if err != nil {
		d.markAttachmentUnhealthy(workspace.ID, attachment.ID, err)
		if workspace.RemoteHost != "" {
			remoteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_attachment", attachmentUpdateRequest{
				Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
				Status: AttachmentUnhealthy, Error: err.Error(),
			})
			cancel()
		}
		return
	}
	closed := closedPaneIDs(workspace, attachment, views)
	if len(closed) != 0 {
		request := closePanesRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			ExpectedRevision: workspace.Revision, Panes: closed,
			Manifest: manifest, Views: views,
		}
		if workspace.RemoteHost != "" {
			remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "close_panes", request)
			cancel()
		} else {
			_, err = d.closePanes(ctx, request)
		}
		if err != nil {
			if strings.Contains(err.Error(), "revision changed") || strings.Contains(err.Error(), "missing ready pane") {
				d.scheduleFreshCapture(endpoint)
			} else {
				d.markAttachmentUnhealthy(workspace.ID, attachment.ID, err)
			}
			return
		}
		for paneID, view := range views {
			if view.Focused {
				if workspace.RemoteHost != "" {
					seenCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					_, _ = d.remotes.Call(seenCtx, workspace.RemoteHost, "seen", workspacePaneRequest{Workspace: workspace.ID, Pane: paneID})
					cancel()
				} else {
					_, _ = d.markSeen(workspace.ID, paneID)
				}
			}
		}
		return
	}
	if workspace.RemoteHost != "" && attachment.Role == AttachmentPrimary && workspace.PrimaryAttachmentID == attachment.ID {
		remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_manifest", manifestUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			ExpectedRevision: workspace.Revision, Manifest: manifest, Views: views,
		})
		cancel()
	} else if attachment.Role == AttachmentPrimary && workspace.PrimaryAttachmentID == attachment.ID {
		_, err = d.updateManifest(manifestUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			ExpectedRevision: workspace.Revision, Manifest: manifest, Views: views,
		})
	} else {
		_, err = d.updateAttachment(attachmentUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			ExpectedRevision: workspace.Revision, Status: AttachmentReady, Views: views,
		})
		if workspace.RemoteHost != "" {
			remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err == nil {
				_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_attachment", attachmentUpdateRequest{
					Workspace: workspace.ID, Attachment: attachment.ID,
					ExpectedRevision: workspace.Revision, Status: AttachmentReady, Views: views,
				})
			} else {
				_, _ = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_attachment", attachmentUpdateRequest{
					Workspace: workspace.ID, Attachment: attachment.ID,
					ExpectedRevision: workspace.Revision, Status: AttachmentUnhealthy, Views: views, Error: err.Error(),
				})
			}
			cancel()
		}
	}
	if err != nil && !strings.Contains(err.Error(), "revision changed") {
		d.markAttachmentUnhealthy(workspace.ID, attachment.ID, err)
		return
	}
	for paneID, view := range views {
		if view.Focused {
			if workspace.RemoteHost != "" {
				seenCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, _ = d.remotes.Call(seenCtx, workspace.RemoteHost, "seen", workspacePaneRequest{Workspace: workspace.ID, Pane: paneID})
				cancel()
			} else {
				_, _ = d.markSeen(workspace.ID, paneID)
			}
		}
	}
}

func (d *Daemon) scheduleFreshCapture(endpoint string) {
	d.startWorker(func(ctx context.Context) {
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
			d.scheduleCapture(endpoint)
		}
	})
}

func closedPaneIDs(workspace *Workspace, attachment *Attachment, views map[string]RuntimeView) []string {
	var closed []string
	for paneID := range attachment.Views {
		pane := workspace.Panes[paneID]
		if pane == nil || pane.RemovalPending {
			continue
		}
		if _, present := views[paneID]; !present {
			closed = append(closed, paneID)
		}
	}
	sort.Strings(closed)
	return closed
}

func (d *Daemon) closeEventsCoverWorkspace(endpoint string, closed map[string]bool) bool {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil || attachment.Status != AttachmentReady || attachment.Revoked {
		return false
	}
	active := 0
	for paneID, pane := range workspace.Panes {
		if pane.RemovalPending {
			continue
		}
		if _, owned := attachment.Views[paneID]; !owned {
			return false
		}
		active++
		if !closed[paneID] {
			return false
		}
	}
	return active != 0
}

func (d *Daemon) scheduleEndpointDeletion(endpoint string) {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil || attachment.Status == AttachmentDetached || attachment.Revoked {
		return
	}
	d.startWorker(func(ctx context.Context) {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if workspace.RemoteHost != "" {
			_, err := d.remotes.Call(callCtx, workspace.RemoteHost, "kill_workspace", killWorkspaceRequest{WorkspaceID: workspace.ID})
			if err != nil {
				d.logger.Printf("delete remote workspace %s after Kitty close: %v", workspace.ID, err)
			}
			return
		}
		if _, err := d.killWorkspace(callCtx, workspace.ID); err != nil {
			d.logger.Printf("delete workspace %s after Kitty close: %v", workspace.ID, err)
		}
	})
}

func (d *Daemon) markAttachmentUnhealthy(workspaceRef, attachmentID string, cause error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil || attachment.Status == AttachmentDetached || attachment.Revoked {
		return
	}
	attachment.Status = AttachmentUnhealthy
	attachment.LastError = cause.Error()
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	_ = d.store.Save(d.state)
}

func (d *Daemon) markRevocationClosed(workspaceRef, attachmentID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return
	}
	if attachment := workspace.Attachments[attachmentID]; attachment != nil {
		attachment.RevocationClosed = true
		attachment.UpdatedAt = time.Now().UTC()
		workspace.UpdatedAt = attachment.UpdatedAt
		_ = d.store.Save(d.state)
	}
}

// reconcile is retained as a synchronous diagnostic hook.
func (d *Daemon) reconcile(ctx context.Context) {
	for _, endpoint := range d.attachmentEndpoints() {
		d.captureEndpoint(ctx, endpoint)
	}
}
