package zka

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

const (
	// After this many consecutive failed enforcement passes for one generation,
	// the observed tree is adopted as the desired one. This is the liveness
	// guarantee: because adoption installs exactly what was seen, the next pass
	// converges by construction, so no desired topology can be permanently
	// unsatisfiable.
	maxEnforceAttempts = 3
	backoffBase        = 500 * time.Millisecond
	backoffCap         = 60 * time.Second
)

// endpointBackoff is deliberately in-memory only. Persisting attempt counts
// would put them in attachmentRuntimeEqual and the remote fingerprint, turning
// every failed pass into a state write and a remote push -- recreating the idle
// update storm a previous fix removed.
type endpointBackoff struct {
	attempts    int
	generation  uint64
	nextAttempt time.Time
}

func backoffDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := backoffBase
	for i := 1; i < attempts && delay < backoffCap; i++ {
		delay *= 2
	}
	if delay > backoffCap {
		delay = backoffCap
	}
	// Equal jitter: half fixed, half random, so attachments that failed
	// together do not retry in lockstep.
	half := delay / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func (d *Daemon) noteTopologyFailure(endpoint string, generation uint64) int {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	state := d.backoff[endpoint]
	if state == nil || state.generation != generation {
		state = &endpointBackoff{generation: generation}
		d.backoff[endpoint] = state
	}
	state.attempts++
	state.nextAttempt = time.Now().Add(backoffDelay(state.attempts))
	return state.attempts
}

func (d *Daemon) clearTopologyBackoff(endpoint string) {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	delete(d.backoff, endpoint)
}

func (d *Daemon) topologyBackoffReady(endpoint string) (bool, time.Duration) {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	state := d.backoff[endpoint]
	if state == nil || state.nextAttempt.IsZero() {
		return true, 0
	}
	if wait := time.Until(state.nextAttempt); wait > 0 {
		return false, wait
	}
	return true, 0
}

func (d *Daemon) scheduleTopologyReconcile(endpoint string) {
	if endpoint == "" {
		return
	}
	if ready, wait := d.topologyBackoffReady(endpoint); !ready {
		d.armDeferredReconcile(endpoint, wait)
		return
	}
	d.topologyMu.Lock()
	if d.reconciling[endpoint] {
		d.reconcileAgain[endpoint] = true
		d.topologyMu.Unlock()
		return
	}
	d.reconciling[endpoint] = true
	d.captureHold[endpoint]++
	d.topologyMu.Unlock()
	d.startWorker(func(ctx context.Context) {
		defer func() {
			d.topologyMu.Lock()
			again := d.reconcileAgain[endpoint]
			delete(d.reconcileAgain, endpoint)
			delete(d.reconciling, endpoint)
			if d.captureHold[endpoint] > 0 {
				d.captureHold[endpoint]--
			}
			if d.captureHold[endpoint] == 0 {
				delete(d.captureHold, endpoint)
			}
			d.topologyMu.Unlock()
			if again && ctx.Err() == nil {
				d.scheduleTopologyReconcile(endpoint)
			}
		}()
		d.runTopologyReconcile(ctx, endpoint)
	})
}

// armDeferredReconcile schedules exactly one wake-up per endpoint while a
// backoff window is open, so repeated triggers coalesce instead of spinning.
func (d *Daemon) armDeferredReconcile(endpoint string, wait time.Duration) {
	d.topologyMu.Lock()
	if d.deferredReconcile[endpoint] {
		d.topologyMu.Unlock()
		return
	}
	d.deferredReconcile[endpoint] = true
	d.topologyMu.Unlock()
	d.startWorker(func(ctx context.Context) {
		defer func() {
			d.topologyMu.Lock()
			delete(d.deferredReconcile, endpoint)
			d.topologyMu.Unlock()
		}()
		select {
		case <-ctx.Done():
		case <-time.After(wait):
			if d.endpointNeedsTopologyReconcile(endpoint) {
				d.scheduleTopologyReconcile(endpoint)
			}
		}
	})
}

func (d *Daemon) runTopologyReconcile(ctx context.Context, endpoint string) {
	err := d.reconcileEndpointTopology(ctx, endpoint)
	if ctx.Err() != nil {
		return
	}
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil {
		return
	}
	if err == nil {
		d.clearTopologyBackoff(endpoint)
		return
	}
	attempts := d.noteTopologyFailure(endpoint, workspace.Topology.Generation)
	switch classifyReconcileError(err, attempts, maxEnforceAttempts) {
	case reconcileFatal:
		d.markAttachmentReconcileError(workspace.ID, attachment.ID, err)
	case reconcileRetryFast:
		d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
		d.clearTopologyBackoff(endpoint)
		d.startWorker(func(retryCtx context.Context) {
			select {
			case <-retryCtx.Done():
			case <-time.After(100 * time.Millisecond):
				d.scheduleTopologyReconcile(endpoint)
			}
		})
	case reconcileAdopt:
		d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
		if adoptErr := d.adoptObservedTopology(ctx, endpoint); adoptErr != nil {
			d.logger.Printf("adopt observed topology at %s: %v", endpoint, adoptErr)
			return
		}
		d.clearTopologyBackoff(endpoint)
		d.scheduleTopologyReconcile(endpoint)
	default:
		d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
	}
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
		// An explicit request is a fresh intent, so any backoff or terminal
		// error left by earlier automatic attempts is cleared first.
		d.clearTopologyBackoff(attachment.Endpoint)
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
			if attachment.ReconcileStatus == TopologyReconcileError {
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

// topologyCaptureSuppressed reports whether captures are held off. The hold is
// refcounted rather than deadline-based: the previous code unconditionally
// reset it to 250ms when a reconcile finished, silently discarding the settle
// window that a session reload had just installed.
func (d *Daemon) topologyCaptureSuppressed(endpoint string) bool {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	if d.captureHold[endpoint] > 0 {
		return true
	}
	until := d.captureHoldUntil[endpoint]
	if until.IsZero() || time.Now().After(until) {
		delete(d.captureHoldUntil, endpoint)
		return false
	}
	return true
}

func (d *Daemon) extendTopologyCaptureSuppression(endpoint string, duration time.Duration) {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	until := time.Now().Add(duration)
	if d.captureHoldUntil[endpoint].Before(until) {
		d.captureHoldUntil[endpoint] = until
	}
}

func (d *Daemon) endpointNeedsTopologyReconcile(endpoint string) bool {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil || len(workspace.Topology.Roots) == 0 {
		return false
	}
	// "error" is terminal until something changes. A fatal reconcile means the
	// desired state is malformed, and re-running it on a timer only repeats the
	// damage; every other status stays retryable.
	if attachment.ReconcileStatus == TopologyReconcileError {
		return false
	}
	return attachment.AppliedTopologyGeneration != workspace.Topology.Generation ||
		attachment.AppliedTopologyDigest != workspace.Topology.Digest ||
		attachment.ReconcileStatus == TopologyReconcilePending
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
	if attachment.ReconcileStatus == TopologyReconcileApplying && attachment.ReconcileTargetGeneration == generation {
		return
	}
	attachment.Status = AttachmentPreparing
	attachment.ReconcileStatus = TopologyReconcileApplying
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
	if attachment.Status == AttachmentUnhealthy && attachment.ReconcileStatus == TopologyReconcileError && attachment.LastError == message {
		return
	}
	attachment.Status = AttachmentUnhealthy
	attachment.ReconcileStatus = TopologyReconcileError
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
		return nil, nil, fmt.Errorf("%w: list windows: %v", errKittyCommand, err)
	}
	views, untagged := findWorkspaceViews(tree, workspace.ID)
	if foreign := foreignUntaggedWindows(untagged); len(foreign) != 0 {
		return nil, nil, fmt.Errorf("%w: untagged windows %v", errKittyNotQuiescent, foreign)
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
	for paneID, view := range views {
		pane := workspace.Panes[paneID]
		if pane == nil {
			// A window tagged for this workspace whose pane we do not know is
			// closed rather than treated as permanent damage; leaving it in
			// place used to wedge every subsequent capture.
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.CloseWindow(callCtx, endpoint, view.WindowID)
			cancel()
			return fmt.Errorf("%w: %s", errUnknownCapturedPane, paneID)
		}
		if pane.Proposed() {
			// Admission owns this transition; retrying the reconcile against a
			// half-created pane just fights it.
			d.schedulePaneAdmission(endpoint)
			return fmt.Errorf("%w: pane %s", errPaneAdmissionPending, paneID)
		}
	}
	if _, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration); err != nil {
		return err
	}

	plan := planTopologyReconcile(workspace, nodes, views, focusedPane)
	if !plan.empty() {
		if err := d.applyTopologyPlan(ctx, endpoint, workspace, attachment, plan); err != nil {
			return err
		}
		if plan.structural() {
			if err := d.waitForDesiredPanes(ctx, endpoint, workspace, 10*time.Second); err != nil {
				return err
			}
		}
		nodes, _, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
		if err != nil {
			return err
		}
	}

	current, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration)
	if err != nil {
		return err
	}
	if !topologyMatchesDesired(current, nodes) {
		// Not a permanent failure: after maxEnforceAttempts this classifies as
		// "adopt", and the observed tree becomes the desired one.
		return fmt.Errorf("%w: generation %d", errStructureNotConverged, targetGeneration)
	}

	manifest, capturedViews, err := d.captureConvergedManifest(ctx, endpoint, workspace, targetGeneration)
	if err != nil {
		return err
	}
	updated, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision: workspace.Revision, BaseTopologyGeneration: targetGeneration,
		VerifyTopologyGeneration: targetGeneration,
		Manifest:                 manifest, Views: capturedViews,
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

func (d *Daemon) captureConvergedManifest(ctx context.Context, endpoint string, workspace *Workspace, generation uint64) (Manifest, map[string]RuntimeView, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		manifest, views, err := CaptureManifest(callCtx, d.kitty, endpoint, workspace)
		cancel()
		if err == nil {
			stable, stableErr := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, manifest.Topology)
			if stableErr == nil && topologyMatchesDesired(workspace, stable) {
				manifest.Topology = stable
				return manifest, views, nil
			}
			if stableErr != nil {
				err = stableErr
			} else {
				err = fmt.Errorf("%w: generation %d", errStructureNotConverged, generation)
			}
		}
		if time.Now().After(deadline) {
			return Manifest{}, nil, err
		}
		select {
		case <-ctx.Done():
			return Manifest{}, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// applyTopologyPlan performs exactly the operations the plan calls for. Nothing
// here is unconditional: a converged workspace produces an empty plan and so
// issues no Kitty commands at all. There is no whole-session rebuild -- every
// structural difference is expressible as launch, close, detach-window or
// detach-tab.
func (d *Daemon) applyTopologyPlan(ctx context.Context, endpoint string, workspace *Workspace, attachment *Attachment, plan topologyPlan) error {
	sort.Slice(plan.Close, func(i, j int) bool { return plan.Close[i] < plan.Close[j] })
	for _, windowID := range plan.Close {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.CloseWindow(callCtx, endpoint, windowID)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: close obsolete window %d: %v", errKittyCommand, windowID, err)
		}
	}

	transport := attachment.Transport
	if workspace.RemoteHost != "" {
		transport = Transport{Kind: "ssh", Host: workspace.RemoteHost}
	}
	for _, target := range plan.Launch {
		pane := workspace.Panes[target.PaneID]
		if pane == nil || !pane.Admitted() {
			return fmt.Errorf("%w: desired pane %s is not launchable", errTopologyInvalid, target.PaneID)
		}
		if target.Anchor == 0 {
			return fmt.Errorf("%w: cannot seed an empty Kitty process", errKittyNotQuiescent)
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := d.kitty.LaunchPane(callCtx, endpoint, workspace, pane, transport, attachment.ID,
			target.OSNodeID, target.TabNodeID, target.LaunchType, target.Anchor)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: launch missing pane %s: %v", errKittyCommand, pane.ID, err)
		}
	}

	// Moves shift the tree underneath themselves, so each step re-reads the
	// runtime ids it needs.
	if len(plan.MoveWindows) != 0 {
		anchorTab := int64(0)
		for _, move := range plan.MoveWindows {
			_, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
			if err != nil {
				return err
			}
			view, ok := views[move.PaneID]
			if !ok {
				return fmt.Errorf("%w: pane %s vanished while regrouping", errKittyNotQuiescent, move.PaneID)
			}
			target := int64(0)
			if move.TargetTabID == -1 {
				target = anchorTab
				if target != 0 && view.TabID == target {
					continue
				}
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = d.kitty.DetachWindow(callCtx, endpoint, view.WindowID, target)
			cancel()
			if err != nil {
				return fmt.Errorf("%w: move pane %s: %v", errKittyCommand, move.PaneID, err)
			}
			if anchorTab == 0 {
				_, views, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
				if err != nil {
					return err
				}
				anchorTab = views[move.PaneID].TabID
			}
		}
	}

	if len(plan.MoveTabs) != 0 {
		anchorTab := int64(0)
		for _, move := range plan.MoveTabs {
			target := int64(0)
			if move.TargetTabID == -1 {
				target = anchorTab
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.DetachTab(callCtx, endpoint, move.TabID, target)
			cancel()
			if err != nil {
				return fmt.Errorf("%w: move tab %d: %v", errKittyCommand, move.TabID, err)
			}
			if anchorTab == 0 {
				anchorTab = move.TabID
			}
		}
	}

	for _, action := range plan.TabLayouts {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.SetEnabledLayouts(callCtx, endpoint, action.TabID, action.Enabled)
		if err == nil {
			err = d.kitty.GotoLayout(callCtx, endpoint, action.TabID, action.Layout)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("%w: apply layout to tab %s: %v", errKittyCommand, action.TabNodeID, err)
		}
	}
	for _, action := range plan.TabTitles {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.SetTabTitle(callCtx, endpoint, action.TabID, action.Title)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: apply title to tab %s: %v", errKittyCommand, action.TabNodeID, err)
		}
	}
	if plan.Focus != "" {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = d.kitty.FocusPane(callCtx, endpoint, workspace.ID, plan.Focus)
		cancel()
	}
	return nil
}

// adoptObservedTopology installs what Kitty actually shows as the new desired
// topology. It is what makes non-convergence self-limiting: whatever the reason
// enforcement kept failing, the next pass starts from a tree that is
// reproducible by definition. Adoption may regroup or reorder; it may never
// change the pane set.
func (d *Daemon) adoptObservedTopology(ctx context.Context, endpoint string) error {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	manifest, views, err := CaptureManifest(callCtx, d.kitty, endpoint, workspace)
	cancel()
	if err != nil {
		return err
	}
	if workspace.RemoteHost != "" {
		// A remote-cached workspace may not install a desired topology itself,
		// so the observed tree goes to the origin, which adopts it and pushes
		// the new generation back. Without this a replica can never escape.
		d.logger.Printf("forwarding observed Kitty topology for workspace %s to origin %s",
			shortID(workspace.ID), workspace.RemoteHost)
		remoteCtx, remoteCancel := context.WithTimeout(ctx, 10*time.Second)
		defer remoteCancel()
		_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_manifest", manifestUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			BaseTopologyGeneration: attachment.AppliedTopologyGeneration,
			Manifest:               manifest, Views: views,
		})
		return err
	}
	stable, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, manifest.Topology)
	if err != nil {
		return err
	}
	if !samePaneSet(topologyPaneIDs(stable), activeTopologyPaneIDs(workspace)) {
		return fmt.Errorf("refusing to adopt a topology whose pane set differs from the workspace")
	}
	manifest.Topology = stable
	d.logger.Printf("adopting observed Kitty topology for workspace %s at %s after %d failed enforcement passes",
		shortID(workspace.ID), endpoint, maxEnforceAttempts)
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision: workspace.Revision, BaseTopologyGeneration: workspace.Topology.Generation,
		Manifest: manifest, Views: views,
	})
	return err
}

func (d *Daemon) workspaceAtTopologyGeneration(workspaceID string, generation uint64) (*Workspace, error) {
	workspace, err := d.getWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if workspace.Topology.Generation != generation {
		return nil, fmt.Errorf("%w: have %d, reconciling %d",
			errTopologyGenerationChanged, workspace.Topology.Generation, generation)
	}
	return workspace, nil
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
		return fmt.Errorf("%w: wait for reconciled panes", errPanesNotReady)
	}
	return fmt.Errorf("%w: wait for reconciled panes: %v", errPanesNotReady, lastErr)
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

// topologyReconcileErrorIsTransient survives for callers that only need the
// coarse question. The taxonomy lives in classifyReconcileError.
func topologyReconcileErrorIsTransient(err error) bool {
	return err != nil && !errors.Is(err, errTopologyInvalid)
}
