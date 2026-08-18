package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// After this many consecutive failed enforcement passes for one generation,
	// automatic layout reconciliation stalls for that attachment. The desired
	// tree remains authoritative until a later generation or an explicit
	// operator-confirmed adoption re-arms it.
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

func (d *Daemon) endpointTopologyOperation(endpoint string) *sync.Mutex {
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	operation := d.topologyOps[endpoint]
	if operation == nil {
		operation = &sync.Mutex{}
		d.topologyOps[endpoint] = operation
	}
	return operation
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
		operation := d.endpointTopologyOperation(endpoint)
		operation.Lock()
		defer operation.Unlock()
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
	if (errors.Is(err, errStructureNotConverged) || errors.Is(err, errStructuralApplyFailed)) &&
		!errors.Is(err, errTopologyGenerationChanged) {
		attempts = d.recordAttachmentReconcileFailure(workspace.ID, attachment.ID, workspace.Topology.Generation, err)
	}
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
	case reconcileStall:
		d.markAttachmentReconcileError(workspace.ID, attachment.ID, err)
	default:
		d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
	}
}

func (d *Daemon) recordAttachmentReconcileFailure(workspaceID, attachmentID string, generation uint64, failure error) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return maxEnforceAttempts
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return maxEnforceAttempts
	}
	if attachment.ReconcileFailureGeneration != generation {
		attachment.ReconcileFailureGeneration = generation
		attachment.ReconcileFailures = 0
	}
	attachment.ReconcileFailures++
	attachment.LastError = failure.Error()
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	if err := d.store.Save(d.state); err != nil {
		d.logger.Printf("persist topology failure for attachment %s: %v", attachmentID, err)
	}
	return attachment.ReconcileFailures
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
		d.clearAttachmentReconcileFailures(workspaceID, attachment.ID)
		if err := d.markAttachmentReconciling(workspaceID, attachment.ID, targetGeneration); err != nil {
			return nil, err
		}
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
	if attachment.ReconcileStatus == TopologyReconcileError &&
		attachment.ReconcileTargetGeneration == workspace.Topology.Generation {
		return false
	}
	return attachment.AppliedTopologyGeneration != workspace.Topology.Generation ||
		attachment.AppliedTopologyDigest != workspace.Topology.Digest ||
		attachment.ReconcileStatus == TopologyReconcilePending ||
		attachment.ReconcileStatus == TopologyReconcileApplying ||
		attachment.ReconcileStatus == TopologyReconcileError
}

func (d *Daemon) markAttachmentReconciling(workspaceID, attachmentID string, generation uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return nil
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return nil
	}
	if attachment.ReconcileStatus == TopologyReconcileApplying && attachment.ReconcileTargetGeneration == generation {
		return nil
	}
	before := attachment.Clone()
	previousUpdatedAt := workspace.UpdatedAt
	if attachment.ReconcileFailureGeneration != generation {
		attachment.ReconcileFailureGeneration = generation
		attachment.ReconcileFailures = 0
	}
	attachment.Status = AttachmentPreparing
	attachment.ReconcileStatus = TopologyReconcileApplying
	attachment.ReconcileTargetGeneration = generation
	attachment.LastError = ""
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	if err := d.store.Save(d.state); err != nil {
		workspace.Attachments[attachmentID] = before
		workspace.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (d *Daemon) clearAttachmentReconcileFailures(workspaceID, attachmentID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Attachments[attachmentID] == nil {
		return
	}
	attachment := workspace.Attachments[attachmentID]
	attachment.ReconcileFailures = 0
	attachment.ReconcileFailureGeneration = workspace.Topology.Generation
	if attachment.ReconcileStatus == TopologyReconcileError {
		attachment.ReconcileStatus = TopologyReconcilePending
		attachment.Status = AttachmentPreparing
		attachment.LastError = ""
	}
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
	if attachment.Status == AttachmentLayoutStalled && attachment.ReconcileStatus == TopologyReconcileError && attachment.LastError == message {
		return
	}
	attachment.Status = AttachmentLayoutStalled
	attachment.ReconcileStatus = TopologyReconcileError
	attachment.ReconcileTargetGeneration = workspace.Topology.Generation
	attachment.ReconcileFailureGeneration = workspace.Topology.Generation
	if attachment.ReconcileFailures < maxEnforceAttempts {
		attachment.ReconcileFailures = maxEnforceAttempts
	}
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
	if attachment.ReconcileFailureGeneration != workspace.Topology.Generation {
		attachment.ReconcileFailureGeneration = workspace.Topology.Generation
		attachment.ReconcileFailures = 0
	}
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

func projectReconcileObserved(workspace *Workspace, nodes []Node, views map[string]RuntimeView) ([]Node, map[string]RuntimeView, bool, error) {
	keep := desiredPaneIDs(workspace)
	filteredViews := cloneViews(views)
	hasProposed := false
	for paneID := range views {
		pane := workspace.Panes[paneID]
		if pane != nil && pane.Proposed() {
			hasProposed = true
			delete(filteredViews, paneID)
		}
	}
	projected := projectTopologyPanes(nodes, keep)
	projected, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, projected)
	if err != nil {
		return nil, nil, false, err
	}
	annotateRuntimeViews(projected, filteredViews)
	return projected, filteredViews, hasProposed, nil
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
	if err := d.markAttachmentReconciling(workspace.ID, attachment.ID, targetGeneration); err != nil {
		return fmt.Errorf("persist reconcile start: %w", err)
	}

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
	}
	nodes, views, hasProposed, err := projectReconcileObserved(workspace, nodes, views)
	if err != nil {
		return err
	}
	if _, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration); err != nil {
		return err
	}

	plan := planTopologyReconcile(workspace, nodes, views, focusedPane)
	if !plan.empty() {
		if err := d.applyTopologyPlan(ctx, endpoint, workspace, attachment, plan); err != nil {
			if plan.structural() {
				return fmt.Errorf("%w: %w", errStructuralApplyFailed, err)
			}
			return err
		}
		if plan.structural() {
			if err := d.waitForDesiredPanes(ctx, endpoint, workspace, 10*time.Second); err != nil {
				return fmt.Errorf("%w: %w", errStructuralApplyFailed, err)
			}
		}
		var observedViews map[string]RuntimeView
		nodes, observedViews, err = observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
		if err != nil {
			return err
		}
		var newlyProposed bool
		nodes, views, newlyProposed, err = projectReconcileObserved(workspace, nodes, observedViews)
		if err != nil {
			return err
		}
		hasProposed = hasProposed || newlyProposed
	}

	current, err := d.workspaceAtTopologyGeneration(workspace.ID, targetGeneration)
	if err != nil {
		return err
	}
	if !topologyMatchesDesired(current, nodes) {
		// A repeated mismatch stalls this attachment for the current generation;
		// it never grants the observed intermediate tree publication authority.
		return fmt.Errorf("%w: generation %d", errStructureNotConverged, targetGeneration)
	}
	if err := d.refreshTopologyIdentities(ctx, endpoint, current, views); err != nil {
		return err
	}
	if hasProposed {
		d.schedulePaneAdmission(endpoint)
		return fmt.Errorf("%w: admitted-pane projection converged", errPaneAdmissionPending)
	}

	manifest, capturedViews, err := d.captureConvergedManifest(ctx, endpoint, workspace, targetGeneration)
	if err != nil {
		return err
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision:         workspace.Revision,
		VerifyTopologyGeneration: targetGeneration,
		Manifest:                 manifest, Views: capturedViews,
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateVerify)
	updated, err := d.updateManifest(request)
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

func (d *Daemon) refreshTopologyIdentities(ctx context.Context, endpoint string, workspace *Workspace, views map[string]RuntimeView) error {
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			for _, paneNode := range tabNode.Children {
				view, ok := views[paneNode.PaneID]
				if !ok {
					return fmt.Errorf("%w: pane %s missing while refreshing topology identity", errViewsNotReady, paneNode.PaneID)
				}
				if view.TabNodeID == tabNode.ID && view.OSWindowNodeID == osNode.ID {
					continue
				}
				// Legacy/unlabelled panes remain recoverable through descendant-pane
				// stabilisation. Avoid turning every otherwise converged legacy pass
				// into a write solely to introduce optional labels.
				if view.TabNodeID == "" && view.OSWindowNodeID == "" {
					continue
				}
				callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := d.kitty.SetTopologyIdentity(callCtx, endpoint, view.WindowID, tabNode.ID, osNode.ID)
				cancel()
				if err != nil {
					return fmt.Errorf("%w: refresh pane %s topology identity: %v", errKittyCommand, paneNode.PaneID, err)
				}
			}
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
	ensureTarget := func() error {
		current, err := d.getWorkspace(workspace.ID)
		if err != nil {
			return err
		}
		// Some focused unit tests plan against an uncommitted synthetic target.
		if current.Topology.Generation == 0 {
			return nil
		}
		if current.Topology.Generation != workspace.Topology.Generation || current.Topology.Digest != workspace.Topology.Digest {
			return fmt.Errorf("%w: structural target changed before Kitty command", errTopologyGenerationChanged)
		}
		return nil
	}
	sort.Slice(plan.Close, func(i, j int) bool { return plan.Close[i] < plan.Close[j] })
	for _, windowID := range plan.Close {
		if err := ensureTarget(); err != nil {
			return err
		}
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
		if err := ensureTarget(); err != nil {
			return err
		}
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
		for _, move := range plan.MoveWindows {
			if err := ensureTarget(); err != nil {
				return err
			}
			_, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
			if err != nil {
				return err
			}
			view, ok := views[move.PaneID]
			if !ok {
				return fmt.Errorf("%w: pane %s vanished while regrouping", errKittyNotQuiescent, move.PaneID)
			}
			target := int64(0)
			if move.TargetPaneID != "" {
				targetView, present := views[move.TargetPaneID]
				if !present {
					return fmt.Errorf("%w: target pane %s vanished while regrouping", errKittyNotQuiescent, move.TargetPaneID)
				}
				target = targetView.TabID
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
		}
	}

	if len(plan.MoveTabs) != 0 {
		for _, move := range plan.MoveTabs {
			if err := ensureTarget(); err != nil {
				return err
			}
			_, views, err := observeWorkspaceTopology(ctx, d.kitty, endpoint, workspace)
			if err != nil {
				return err
			}
			view, present := views[move.PaneID]
			if !present {
				return fmt.Errorf("%w: pane %s vanished while regrouping tabs", errKittyNotQuiescent, move.PaneID)
			}
			target := int64(0)
			if move.TargetPaneID != "" {
				targetView, ok := views[move.TargetPaneID]
				if !ok {
					return fmt.Errorf("%w: target pane %s vanished while regrouping tabs", errKittyNotQuiescent, move.TargetPaneID)
				}
				target = targetView.TabID
				if view.OSWindowID == targetView.OSWindowID {
					continue
				}
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = d.kitty.DetachTab(callCtx, endpoint, view.TabID, target)
			cancel()
			if err != nil {
				return fmt.Errorf("%w: move tab containing pane %s: %v", errKittyCommand, move.PaneID, err)
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

func topologyAdoptToken(generation uint64, desiredDigest, candidateDigest string) string {
	return strconv.FormatUint(generation, 10) + ":" + desiredDigest + ":" + candidateDigest
}

func parseTopologyAdoptToken(token string) (uint64, string, string, error) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return 0, "", "", fmt.Errorf("invalid layout adoption confirmation token")
	}
	generation, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, "", "", fmt.Errorf("invalid layout adoption confirmation token")
	}
	return generation, parts[1], parts[2], nil
}

func topologyShape(nodes []Node) string {
	roots := make([]string, 0, len(nodes))
	for _, osNode := range nodes {
		tabs := make([]string, 0, len(osNode.Children))
		for _, tabNode := range osNode.Children {
			panes := make([]string, 0, len(tabNode.Children))
			for _, paneNode := range tabNode.Children {
				panes = append(panes, shortID(paneNode.PaneID))
			}
			tabs = append(tabs, "["+strings.Join(panes, ",")+"]")
		}
		roots = append(roots, "["+strings.Join(tabs, ",")+"]")
	}
	return strings.Join(roots, " ")
}

func (d *Daemon) adoptionAttachment(workspaceRef, attachmentID string) (*Workspace, *Attachment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return nil, nil, err
	}
	choose := func(attachment *Attachment) bool {
		return isLocalUnixAttachment(attachment, d.state.Node.ID) &&
			attachment.Status != AttachmentDetached && !attachment.Revoked
	}
	if attachmentID != "" {
		attachment := workspace.Attachments[attachmentID]
		if !choose(attachment) {
			return nil, nil, fmt.Errorf("attachment %q is not an active local Kitty attachment", attachmentID)
		}
		return workspace.Clone(), attachment.Clone(), nil
	}
	for _, attachment := range workspace.SortedAttachments() {
		if choose(attachment) {
			return workspace.Clone(), attachment.Clone(), nil
		}
	}
	return nil, nil, fmt.Errorf("workspace has no active local Kitty attachment")
}

// adoptLayout is the only path that may deliberately replace an unreachable
// desired layout with an observed one. Preview and confirmation are separate
// captures; the token binds the inspected candidate to the exact desired base.
func (d *Daemon) adoptLayout(ctx context.Context, req topologyAdoptRequest) (topologyAdoptResponse, error) {
	workspace, attachment, err := d.adoptionAttachment(req.Workspace, req.Attachment)
	if err != nil {
		return topologyAdoptResponse{}, err
	}
	operation := d.endpointTopologyOperation(attachment.Endpoint)
	operation.Lock()
	defer operation.Unlock()
	workspace, attachment, err = d.adoptionAttachment(workspace.ID, attachment.ID)
	if err != nil {
		return topologyAdoptResponse{}, err
	}
	for _, pane := range workspace.Panes {
		if pane.Proposed() || pane.Retiring() {
			return topologyAdoptResponse{}, fmt.Errorf("layout adoption requires every pane to be fully admitted")
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	manifest, views, err := CaptureManifest(callCtx, d.kitty, attachment.Endpoint, workspace)
	cancel()
	if err != nil {
		return topologyAdoptResponse{}, err
	}
	stable, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, manifest.Topology)
	if err != nil {
		return topologyAdoptResponse{}, err
	}
	stable = canonicalTopology(stable)
	if !samePaneSet(topologyPaneIDs(stable), activeTopologyPaneIDs(workspace)) {
		return topologyAdoptResponse{}, fmt.Errorf("refusing to adopt a topology whose pane set differs from the workspace")
	}
	candidateDigest := topologyStructuralDigest(stable)
	response := topologyAdoptResponse{
		Workspace: workspace, DesiredDigest: workspace.Topology.Digest, CandidateDigest: candidateDigest,
		DesiredShape: topologyShape(workspace.Topology.Roots), CandidateShape: topologyShape(stable),
	}
	if req.Confirm == "" {
		response.ConfirmToken = topologyAdoptToken(workspace.Topology.Generation, workspace.Topology.Digest, candidateDigest)
		return response, nil
	}
	baseGeneration, baseDigest, confirmedCandidate, err := parseTopologyAdoptToken(req.Confirm)
	if err != nil {
		return topologyAdoptResponse{}, err
	}
	if baseGeneration != workspace.Topology.Generation || baseDigest != workspace.Topology.Digest {
		return topologyAdoptResponse{}, fmt.Errorf("desired topology changed after layout adoption preview")
	}
	if confirmedCandidate != candidateDigest {
		return topologyAdoptResponse{}, fmt.Errorf("observed topology changed after layout adoption preview")
	}
	manifest.Topology = stable
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Manifest: manifest, Views: views, ConfirmedTopologyDigest: candidateDigest,
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateOperator)
	var updated *Workspace
	if workspace.RemoteHost != "" {
		remoteCtx, remoteCancel := context.WithTimeout(ctx, 10*time.Second)
		raw, callErr := d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_manifest", request)
		remoteCancel()
		if callErr != nil {
			return topologyAdoptResponse{}, callErr
		}
		var authoritative Workspace
		if err := json.Unmarshal(raw, &authoritative); err != nil {
			return topologyAdoptResponse{}, fmt.Errorf("decode adopted remote workspace: %w", err)
		}
		updated = &authoritative
	} else {
		updated, err = d.updateManifest(request)
		if err != nil {
			return topologyAdoptResponse{}, err
		}
	}
	response.Workspace = updated
	response.Applied = true
	return response, nil
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
