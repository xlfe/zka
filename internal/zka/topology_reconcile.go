package zka

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func (d *Daemon) scheduleTopologyReconcile(endpoint string) {
	if endpoint == "" {
		return
	}
	d.topologyMu.Lock()
	if d.reconciling[endpoint] {
		d.reconcileAgain[endpoint] = true
		d.topologyMu.Unlock()
		return
	}
	d.reconciling[endpoint] = true
	d.suppressUntil[endpoint] = time.Now().Add(15 * time.Second)
	d.topologyMu.Unlock()
	d.startWorker(func(ctx context.Context) {
		defer func() {
			d.topologyMu.Lock()
			again := d.reconcileAgain[endpoint]
			delete(d.reconcileAgain, endpoint)
			delete(d.reconciling, endpoint)
			d.suppressUntil[endpoint] = time.Now().Add(250 * time.Millisecond)
			d.topologyMu.Unlock()
			if again && ctx.Err() == nil {
				d.scheduleTopologyReconcile(endpoint)
			}
		}()
		if err := d.reconcileEndpointTopology(ctx, endpoint); err != nil && ctx.Err() == nil {
			workspace, attachment := d.endpointAttachment(endpoint)
			if workspace != nil && attachment != nil {
				if topologyReconcileErrorIsTransient(err) {
					d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
					if topologyReconcileRetrySoon(err) {
						d.startWorker(func(retryCtx context.Context) {
							select {
							case <-retryCtx.Done():
							case <-time.After(100 * time.Millisecond):
								d.scheduleTopologyReconcile(endpoint)
							}
						})
					}
				} else {
					d.markAttachmentReconcileError(workspace.ID, attachment.ID, err)
				}
			}
		}
	})
}

func (d *Daemon) resumeTopologyReconciliation() {
	for _, endpoint := range d.attachmentEndpoints() {
		if d.endpointNeedsTopologyReconcile(endpoint) {
			d.scheduleTopologyReconcile(endpoint)
		}
	}
}

func (d *Daemon) reconcileTopology(ctx context.Context, req topologyReconcileRequest) (*Workspace, error) {
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	var attachments []*Attachment
	for _, attachment := range workspace.Attachments {
		if req.Attachment != "" && attachment.ID != req.Attachment {
			continue
		}
		if !isLocalUnixAttachment(attachment, d.state.Node.ID) ||
			attachment.Status == AttachmentDetached || attachment.Revoked {
			continue
		}
		attachments = append(attachments, attachment.Clone())
	}
	workspaceID := workspace.ID
	targetGeneration := workspace.Topology.Generation
	d.mu.Unlock()
	if len(attachments) == 0 {
		if req.Attachment != "" {
			return nil, fmt.Errorf("attachment %q is not an active local Kitty attachment", req.Attachment)
		}
		return nil, fmt.Errorf("workspace has no active local Kitty attachments")
	}
	for _, attachment := range attachments {
		d.markAttachmentReconciling(workspaceID, attachment.ID, targetGeneration)
		d.scheduleTopologyReconcile(attachment.Endpoint)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, getErr := d.getWorkspace(workspaceID)
		if getErr != nil {
			return nil, getErr
		}
		targetGeneration = current.Topology.Generation
		allReady := true
		for _, requested := range attachments {
			attachment := current.Attachments[requested.ID]
			if attachment == nil {
				return nil, fmt.Errorf("attachment %s disappeared during reconciliation", requested.ID)
			}
			if attachment.ReconcileStatus == "error" {
				return nil, fmt.Errorf("reconcile attachment %s: %s", requested.ID, attachment.LastError)
			}
			allReady = allReady && attachment.Status == AttachmentReady &&
				attachment.AppliedTopologyGeneration == targetGeneration &&
				attachment.AppliedTopologyDigest == current.Topology.Digest
		}
		if allReady {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Daemon) topologyCaptureSuppressed(endpoint string) bool {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	until := d.suppressUntil[endpoint]
	if until.IsZero() || time.Now().After(until) {
		delete(d.suppressUntil, endpoint)
		return false
	}
	return true
}

func (d *Daemon) endpointNeedsTopologyReconcile(endpoint string) bool {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil || len(workspace.Topology.Roots) == 0 {
		return false
	}
	return attachment.AppliedTopologyGeneration != workspace.Topology.Generation ||
		attachment.AppliedTopologyDigest != workspace.Topology.Digest ||
		attachment.ReconcileStatus == "pending" ||
		attachment.ReconcileStatus == "error"
}

func (d *Daemon) markAttachmentReconciling(workspaceID, attachmentID string, generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return
	}
	if attachment.ReconcileStatus == "applying" && attachment.ReconcileTargetGeneration == generation {
		return
	}
	attachment.Status = AttachmentPreparing
	attachment.ReconcileStatus = "applying"
	attachment.ReconcileTargetGeneration = generation
	attachment.LastError = ""
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	_ = d.store.Save(d.state)
}

func (d *Daemon) markAttachmentReconcileError(workspaceID, attachmentID string, failure error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return
	}
	message := failure.Error()
	if attachment.Status == AttachmentUnhealthy && attachment.ReconcileStatus == "error" && attachment.LastError == message {
		return
	}
	attachment.Status = AttachmentUnhealthy
	attachment.ReconcileStatus = "error"
	attachment.ReconcileTargetGeneration = workspace.Topology.Generation
	attachment.LastError = message
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	_ = d.store.Save(d.state)
}

func (d *Daemon) markAttachmentReconcilePending(workspaceID, attachmentID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil || attachment.Status == AttachmentDetached || attachment.Revoked {
		return
	}
	if attachment.Status == AttachmentPreparing &&
		attachment.ReconcileStatus == TopologyReconcilePending &&
		attachment.ReconcileTargetGeneration == workspace.Topology.Generation &&
		attachment.LastError == "" {
		return
	}
	attachment.Status = AttachmentPreparing
	attachment.ReconcileStatus = TopologyReconcilePending
	attachment.ReconcileTargetGeneration = workspace.Topology.Generation
	attachment.LastError = ""
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	_ = d.store.Save(d.state)
}

func observeWorkspaceTopology(ctx context.Context, kitty KittyClient, endpoint string, workspace *Workspace) ([]Node, map[string]RuntimeView, error) {
	tree, err := kitty.List(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	views, untagged := findWorkspaceViews(tree, workspace.ID)
	if len(untagged) != 0 {
		return nil, nil, fmt.Errorf("kitty has untagged windows: %v", untagged)
	}
	nodes, err := topologyFromKitty(tree, workspace.ID)
	if err != nil {
		return nil, nil, err
	}
	nodes, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, nodes)
	if err != nil {
		return nil, nil, err
	}
	annotateRuntimeViews(nodes, views)
	return nodes, views, nil
}

func (d *Daemon) reconcileEndpointTopology(ctx context.Context, endpoint string) error {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil {
		return nil
	}
	if attachment.Status == AttachmentDetached || attachment.Revoked || len(workspace.Topology.Roots) == 0 {
		return nil
	}
	targetGeneration := workspace.Topology.Generation
	d.markAttachmentReconciling(workspace.ID, attachment.ID, targetGeneration)

	focusedPane := ""
	for paneID, view := range attachment.Views {
		if view.Focused {
			focusedPane = paneID
			break
		}
	}
	nodes, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
	if err != nil {
		return err
	}
	for paneID := range views {
		pane := workspace.Panes[paneID]
		if pane == nil {
			return fmt.Errorf("Kitty attachment contains unknown pane %s", paneID)
		}
		if pane.TopologyPending {
			return d.commitPendingAttachmentTopology(ctx, workspace, attachment)
		}
	}
	if _, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration); err != nil {
		return err
	}
	desired := desiredPaneIDs(workspace)
	for paneID, view := range views {
		pane := workspace.Panes[paneID]
		if !desired[paneID] || pane.RemovalPending {
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.CloseWindow(callCtx, endpoint, view.WindowID)
			cancel()
			if err != nil {
				return fmt.Errorf("close obsolete pane %s: %w", paneID, err)
			}
		}
	}
	if !samePaneSet(topologyPaneIDs(nodes), desired) {
		if err := d.launchMissingTopologyPanes(ctx, endpoint, workspace, attachment, views); err != nil {
			return err
		}
		if err := d.waitForDesiredPanes(ctx, endpoint, workspace, 10*time.Second); err != nil {
			return err
		}
	}
	if err := d.reconcileTopologyGrouping(ctx, endpoint, workspace); err != nil {
		return err
	}
	if err := d.reconcileTopologyMetadata(ctx, endpoint, workspace); err != nil {
		return err
	}
	current, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration)
	if err != nil {
		return err
	}
	nodes, _, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, current)
	if err != nil {
		return err
	}
	if !topologyMatchesDesired(current, nodes) {
		if err := d.reloadDesiredTopologySession(ctx, endpoint, current, attachment); err != nil {
			return err
		}
	}
	if focusedPane != "" {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = d.kitty.FocusPane(callCtx, endpoint, workspace.ID, focusedPane)
		cancel()
	}

	deadline := time.Now().Add(5 * time.Second)
	var manifest Manifest
	for {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		manifest, views, err = CaptureManifest(callCtx, d.kitty, endpoint, workspace)
		cancel()
		if err == nil {
			stable, stableErr := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, manifest.Topology)
			if stableErr == nil && topologyMatchesDesired(workspace, stable) {
				manifest.Topology = stable
				break
			}
			if stableErr != nil {
				err = stableErr
			} else {
				err = fmt.Errorf("Kitty topology still differs from generation %d", targetGeneration)
			}
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	updated, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision: workspace.Revision, BaseTopologyGeneration: targetGeneration,
		VerifyTopologyGeneration: targetGeneration,
		Manifest:                 manifest, Views: views,
	})
	if err != nil {
		return err
	}
	if workspace.RemoteHost != "" {
		localAttachment := updated.Attachments[attachment.ID]
		if localAttachment == nil {
			return fmt.Errorf("attachment disappeared after topology verification")
		}
		remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, remoteErr := d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_attachment", attachmentUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			TopologyGeneration: localAttachment.AppliedTopologyGeneration,
			TopologyDigest:     localAttachment.AppliedTopologyDigest,
			ObservedTopology:   localAttachment.ObservedTopology,
			Status:             AttachmentReady, Views: localAttachment.Views,
		})
		cancel()
		if remoteErr != nil {
			return remoteErr
		}
	}
	return nil
}

func (d *Daemon) workspaceAtTopologyGeneration(workspaceID string, generation uint64) (*Workspace, error) {
	workspace, err := d.getWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if workspace.Topology.Generation != generation {
		return nil, fmt.Errorf("topology generation changed: have %d, reconciling %d",
			workspace.Topology.Generation, generation)
	}
	return workspace, nil
}

func (d *Daemon) commitPendingAttachmentTopology(ctx context.Context, workspace *Workspace, attachment *Attachment) error {
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	manifest, views, err := CaptureManifest(callCtx, d.kitty, attachment.Endpoint, workspace)
	cancel()
	if err != nil {
		return fmt.Errorf("capture pending pane topology: %w", err)
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision: workspace.Revision, BaseTopologyGeneration: attachment.AppliedTopologyGeneration,
		Manifest: manifest, Views: views,
	}
	if request.BaseTopologyGeneration == 0 {
		request.BaseTopologyGeneration = workspace.Topology.Generation
	}
	if workspace.RemoteHost != "" {
		remoteCtx, remoteCancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_manifest", request)
		remoteCancel()
	} else {
		_, err = d.updateManifest(request)
	}
	if err != nil {
		return fmt.Errorf("commit pending pane topology: %w", err)
	}
	return fmt.Errorf("topology generation changed after pending pane capture")
}

func (d *Daemon) reloadDesiredTopologySession(ctx context.Context, endpoint string, workspace *Workspace, attachment *Attachment) error {
	transport := attachment.Transport
	if workspace.RemoteHost != "" {
		transport = Transport{Kind: "ssh", Host: workspace.RemoteHost}
	}
	session, err := renderDesiredTopologySession(workspace, transport, attachment.ID)
	if err != nil {
		return err
	}
	suffix := fmt.Sprintf("%s-reconcile-%d-%d", shortID(attachment.ID), workspace.Topology.Generation, time.Now().UnixNano())
	path, err := d.store.WriteSession(workspace.ID, suffix, session)
	if err != nil {
		return fmt.Errorf("write reconciliation session: %w", err)
	}
	defer func() { _ = os.Remove(path) }()

	tree, err := d.kitty.List(ctx, endpoint)
	if err != nil {
		return err
	}
	oldWindowIDs := map[int64]bool{}
	anchorWindowID := int64(0)
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] != workspace.ID {
					continue
				}
				paneID := window.UserVars["zka_pane"]
				pane := workspace.Panes[paneID]
				if pane == nil || pane.TopologyPending {
					return fmt.Errorf("pending pane appeared during topology reconciliation")
				}
				oldWindowIDs[window.ID] = true
				if anchorWindowID == 0 {
					anchorWindowID = window.ID
				}
			}
		}
	}
	if anchorWindowID == 0 {
		return fmt.Errorf("cannot reload topology without an existing Kitty window")
	}
	d.extendTopologyCaptureSuppression(endpoint, 30*time.Second)
	loadCtx, loadCancel := context.WithTimeout(ctx, 5*time.Second)
	err = d.kitty.LoadSession(loadCtx, endpoint, path, anchorWindowID)
	loadCancel()
	if err != nil {
		return fmt.Errorf("load desired Kitty session: %w", err)
	}

	selected, err := d.waitForReplacementViews(ctx, endpoint, workspace, oldWindowIDs, 10*time.Second)
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.closeReplacementViews(cleanupCtx, endpoint, workspace.ID, oldWindowIDs, nil)
		cleanupCancel()
		return err
	}
	if _, err := d.workspaceAtTopologyGeneration(workspace.ID, workspace.Topology.Generation); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.closeReplacementViews(cleanupCtx, endpoint, workspace.ID, oldWindowIDs, nil)
		cleanupCancel()
		return err
	}
	if err := d.closeReplacementViews(ctx, endpoint, workspace.ID, nil, selected); err != nil {
		return err
	}
	return nil
}

func (d *Daemon) waitForReplacementViews(ctx context.Context, endpoint string, workspace *Workspace, oldWindowIDs map[int64]bool, timeout time.Duration) (map[int64]bool, error) {
	deadline := time.Now().Add(timeout)
	desired := desiredPaneIDs(workspace)
	for time.Now().Before(deadline) {
		tree, err := d.kitty.List(ctx, endpoint)
		if err == nil {
			selected := map[int64]bool{}
			found := map[string]bool{}
			pending := false
			for _, osWindow := range tree {
				for _, tab := range osWindow.Tabs {
					for _, window := range tab.Windows {
						if window.UserVars["zka_workspace"] != workspace.ID || oldWindowIDs[window.ID] {
							continue
						}
						paneID := window.UserVars["zka_pane"]
						if !desired[paneID] {
							pending = true
							continue
						}
						if desired[paneID] && window.UserVars["zka_ready"] == "1" && !found[paneID] {
							found[paneID] = true
							selected[window.ID] = true
						}
					}
				}
			}
			if pending {
				return nil, fmt.Errorf("pending pane appeared during topology reconciliation")
			}
			if samePaneSet(found, desired) {
				return selected, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("replacement Kitty panes did not become ready")
}

// closeReplacementViews keeps IDs from keepOnly when provided. Otherwise it
// closes only IDs absent from preserve, which is the rollback path.
func (d *Daemon) closeReplacementViews(ctx context.Context, endpoint, workspaceID string, preserve, keepOnly map[int64]bool) error {
	tree, err := d.kitty.List(ctx, endpoint)
	if err != nil {
		return err
	}
	workspace, _ := d.getWorkspace(workspaceID)
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] != workspaceID {
					continue
				}
				if workspace != nil {
					if pane := workspace.Panes[window.UserVars["zka_pane"]]; pane != nil && pane.TopologyPending {
						continue
					}
				}
				closeWindow := keepOnly != nil && !keepOnly[window.ID]
				if keepOnly == nil {
					closeWindow = !preserve[window.ID]
				}
				if !closeWindow {
					continue
				}
				callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				closeErr := d.kitty.CloseWindow(callCtx, endpoint, window.ID)
				cancel()
				if closeErr != nil {
					return fmt.Errorf("close replaced Kitty window %d: %w", window.ID, closeErr)
				}
			}
		}
	}
	return nil
}

func (d *Daemon) extendTopologyCaptureSuppression(endpoint string, duration time.Duration) {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	until := time.Now().Add(duration)
	if d.suppressUntil[endpoint].Before(until) {
		d.suppressUntil[endpoint] = until
	}
}

func (d *Daemon) launchMissingTopologyPanes(ctx context.Context, endpoint string, workspace *Workspace, attachment *Attachment, views map[string]RuntimeView) error {
	transport := attachment.Transport
	if workspace.RemoteHost != "" {
		transport = Transport{Kind: "ssh", Host: workspace.RemoteHost}
	}
	var anyAnchor int64
	for _, view := range views {
		anyAnchor = view.WindowID
		break
	}
	for _, osNode := range workspace.Topology.Roots {
		osAnchor := int64(0)
		for _, tabNode := range osNode.Children {
			for _, paneNode := range tabNode.Children {
				if view, ok := views[paneNode.PaneID]; ok {
					osAnchor = view.WindowID
					break
				}
			}
			if osAnchor != 0 {
				break
			}
		}
		for _, tabNode := range osNode.Children {
			tabAnchor := int64(0)
			for _, paneNode := range tabNode.Children {
				if view, ok := views[paneNode.PaneID]; ok {
					tabAnchor = view.WindowID
					break
				}
			}
			for _, paneNode := range tabNode.Children {
				if _, ok := views[paneNode.PaneID]; ok {
					continue
				}
				pane := workspace.Panes[paneNode.PaneID]
				if pane == nil || pane.RemovalPending || pane.TopologyPending {
					return fmt.Errorf("desired pane %s is not launchable", paneNode.PaneID)
				}
				launchType, anchor := "window", tabAnchor
				if tabAnchor == 0 && osAnchor != 0 {
					launchType, anchor = "tab", osAnchor
				} else if tabAnchor == 0 && osAnchor == 0 {
					if anyAnchor == 0 {
						return fmt.Errorf("cannot seed an empty Kitty process")
					}
					launchType, anchor = "os-window", anyAnchor
				}
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				windowID, err := d.kitty.LaunchPane(callCtx, endpoint, workspace, pane, transport, attachment.ID, osNode.ID, tabNode.ID, launchType, anchor)
				cancel()
				if err != nil {
					return fmt.Errorf("launch missing pane %s: %w", pane.ID, err)
				}
				views[pane.ID] = RuntimeView{PaneID: pane.ID, WindowID: windowID}
				if tabAnchor == 0 {
					tabAnchor = windowID
				}
				if osAnchor == 0 {
					osAnchor = windowID
				}
				if anyAnchor == 0 {
					anyAnchor = windowID
				}
			}
		}
	}
	return nil
}

func (d *Daemon) waitForDesiredPanes(ctx context.Context, endpoint string, workspace *Workspace, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		nodes, views, err := observeWorkspaceTopology(callCtx, d.kitty, endpoint, workspace)
		cancel()
		if err == nil && samePaneSet(topologyPaneIDs(nodes), desiredPaneIDs(workspace)) {
			ready := true
			for paneID := range desiredPaneIDs(workspace) {
				ready = ready && views[paneID].Ready
			}
			if ready {
				return nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("pane clients did not become ready")
	}
	return fmt.Errorf("wait for reconciled panes: %w", lastErr)
}

func desiredTabPanes(workspace *Workspace) [][]string {
	var result [][]string
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			var panes []string
			for _, paneNode := range tabNode.Children {
				panes = append(panes, paneNode.PaneID)
			}
			result = append(result, panes)
		}
	}
	return result
}

func (d *Daemon) reconcileTopologyGrouping(ctx context.Context, endpoint string, workspace *Workspace) error {
	nodes, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
	if err != nil {
		return err
	}
	actualTabMembers := map[int64][]string{}
	for _, osNode := range nodes {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			tabID := views[tabNode.Children[0].PaneID].TabID
			for _, paneNode := range tabNode.Children {
				actualTabMembers[tabID] = append(actualTabMembers[tabID], paneNode.PaneID)
			}
		}
	}
	for _, desiredPanes := range desiredTabPanes(workspace) {
		if len(desiredPanes) == 0 {
			continue
		}
		firstView := views[desiredPanes[0]]
		if topologyStringsEqual(actualTabMembers[firstView.TabID], desiredPanes) {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.DetachWindow(callCtx, endpoint, firstView.WindowID, 0)
		cancel()
		if err != nil {
			return fmt.Errorf("create canonical tab for pane %s: %w", desiredPanes[0], err)
		}
		nodes, views, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
		if err != nil {
			return err
		}
		targetTab := views[desiredPanes[0]].TabID
		for _, paneID := range desiredPanes[1:] {
			if views[paneID].TabID == targetTab {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.DetachWindow(callCtx, endpoint, views[paneID].WindowID, targetTab)
			cancel()
			if err != nil {
				return fmt.Errorf("move pane %s into canonical tab: %w", paneID, err)
			}
			nodes, views, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
			if err != nil {
				return err
			}
			targetTab = views[desiredPanes[0]].TabID
		}
	}
	return d.reconcileOSWindowGrouping(ctx, endpoint, workspace)
}

func (d *Daemon) reconcileOSWindowGrouping(ctx context.Context, endpoint string, workspace *Workspace) error {
	nodes, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
	if err != nil {
		return err
	}
	actualOSMembers := map[int64][]string{}
	for _, osNode := range nodes {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			paneID := tabNode.Children[0].PaneID
			actualOSMembers[views[paneID].OSWindowID] = append(actualOSMembers[views[paneID].OSWindowID], paneID)
		}
	}
	for _, osNode := range workspace.Topology.Roots {
		var tabPanes []string
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) != 0 {
				tabPanes = append(tabPanes, tabNode.Children[0].PaneID)
			}
		}
		if len(tabPanes) == 0 {
			continue
		}
		firstOSWindowID := views[tabPanes[0]].OSWindowID
		if topologyStringsEqual(actualOSMembers[firstOSWindowID], tabPanes) {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.DetachTab(callCtx, endpoint, views[tabPanes[0]].TabID, 0)
		cancel()
		if err != nil {
			return fmt.Errorf("create canonical OS window for pane %s: %w", tabPanes[0], err)
		}
		_, views, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
		if err != nil {
			return err
		}
		targetTab := views[tabPanes[0]].TabID
		for _, paneID := range tabPanes[1:] {
			if views[paneID].OSWindowID == views[tabPanes[0]].OSWindowID {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.DetachTab(callCtx, endpoint, views[paneID].TabID, targetTab)
			cancel()
			if err != nil {
				return fmt.Errorf("move tab containing pane %s into canonical OS window: %w", paneID, err)
			}
			_, views, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
			if err != nil {
				return err
			}
			targetTab = views[tabPanes[0]].TabID
		}
	}
	return nil
}

func (d *Daemon) reconcileTopologyMetadata(ctx context.Context, endpoint string, workspace *Workspace) error {
	_, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
	if err != nil {
		return err
	}
	seenTabs := map[int64]bool{}
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			tabID := views[tabNode.Children[0].PaneID].TabID
			if tabID <= 0 || seenTabs[tabID] {
				continue
			}
			seenTabs[tabID] = true
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.SetTabLayout(callCtx, endpoint, tabID, tabNode.Layout, tabNode.EnabledLayouts)
			cancel()
			if err != nil {
				return fmt.Errorf("apply layout to tab %s: %w", tabNode.ID, err)
			}
			callCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
			err = d.kitty.SetTabTitle(callCtx, endpoint, tabID, tabNode.Title)
			cancel()
			if err != nil {
				return fmt.Errorf("apply title to tab %s: %w", tabNode.ID, err)
			}
		}
	}
	return nil
}

func topologyStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sortedEndpointSet(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func topologyReconcileErrorIsTransient(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "untagged windows") ||
		strings.Contains(message, "did not become ready") ||
		strings.Contains(message, "topology generation changed") ||
		strings.Contains(message, "pending pane appeared")
}

func topologyReconcileRetrySoon(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "topology generation changed") ||
		strings.Contains(message, "pending pane appeared")
}
