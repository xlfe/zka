package zka

import (
	"context"
	"fmt"
	"time"
)

const (
	// How long a proposed pane may be absent from *successful* Kitty listings
	// before it is retired. Only successful listings count, so a Kitty remote
	// control outage can never destroy a pane.
	paneAdmissionGrace = 10 * time.Second
	// How long a proposed pane may wait when its endpoint has no live
	// attachment at all, i.e. Kitty went away entirely.
	paneAdmissionDeadline = 60 * time.Second
	paneAdmissionRetry    = 150 * time.Millisecond
)

// Pane admission replaces a race. Previously a freshly allocated pane was
// fabricated into a synthetic topology tab by a 2s backend ticker whenever its
// zmx session came up before the capture landed -- which is almost always,
// since `zka pane` starts zmx within milliseconds. The fabricated node had no
// enabled_layouts and no layout_state, so the desired topology instantly became
// something Kitty could never reproduce.
//
// Here a pane enters the topology only by committing a capture that already
// contains its tagged window. If the evidence never arrives, the pane is
// retired through the normal cleanup path rather than invented into existence.

func (d *Daemon) schedulePaneAdmission(endpoint string) {
	if endpoint == "" || !d.endpointHasProposedPanes(endpoint) {
		return
	}
	d.captureMu.Lock()
	if d.admitting[endpoint] {
		d.admitAgain[endpoint] = true
		d.captureMu.Unlock()
		return
	}
	d.admitting[endpoint] = true
	d.captureMu.Unlock()
	d.startWorker(func(ctx context.Context) {
		defer func() {
			d.captureMu.Lock()
			again := d.admitAgain[endpoint]
			delete(d.admitAgain, endpoint)
			delete(d.admitting, endpoint)
			d.captureMu.Unlock()
			if again && ctx.Err() == nil {
				d.schedulePaneAdmission(endpoint)
			}
		}()
		operation := d.endpointTopologyOperation(endpoint)
		operation.Lock()
		defer operation.Unlock()
		if err := d.admitPendingPanes(ctx, endpoint); err != nil && ctx.Err() == nil {
			// Admission is best effort and self-healing; the retry is what
			// makes it converge, not an error report.
			d.startWorker(func(retryCtx context.Context) {
				select {
				case <-retryCtx.Done():
				case <-time.After(paneAdmissionRetry):
					if d.endpointHasProposedPanes(endpoint) {
						d.schedulePaneAdmission(endpoint)
					}
				}
			})
		}
	})
}

func (d *Daemon) endpointHasProposedPanes(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, workspace := range d.state.Workspaces {
		if workspace.DeletionPending {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane.Proposed() && pane.Admission.Endpoint == endpoint {
				return true
			}
		}
	}
	return false
}

// admitPendingPanes runs one admission pass. It returns immediately when the
// endpoint owns no proposed pane, which is what keeps an idle daemon from
// issuing any Kitty command at all.
func (d *Daemon) admitPendingPanes(ctx context.Context, endpoint string) error {
	workspace, attachment := d.endpointAttachment(endpoint)
	if workspace == nil || attachment == nil {
		return nil
	}
	if !d.endpointHasProposedPanes(endpoint) {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tree, err := d.kitty.List(listCtx, endpoint)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: list windows for admission: %v", errKittyCommand, err)
	}
	tagged := map[string]bool{}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if paneID := window.UserVars["zka_pane"]; paneID != "" {
					tagged[paneID] = true
				}
			}
		}
	}
	admissible := d.recordAdmissionObservation(workspace.ID, endpoint, tagged)
	if !admissible {
		return errPaneAdmissionPending
	}
	captureCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	manifest, views, err := CaptureManifest(captureCtx, d.kitty, endpoint, workspace)
	cancel()
	if err != nil {
		return fmt.Errorf("capture for pane admission: %w", err)
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		ExpectedRevision: workspace.Revision,
		Manifest:         manifest, Views: views,
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateAdmission)
	request.BaseTopologyGeneration = attachment.AppliedTopologyGeneration
	request.BaseTopologyDigest = attachment.AppliedTopologyDigest
	if request.BaseTopologyGeneration == 0 {
		request.BaseTopologyGeneration = workspace.Topology.Generation
		request.BaseTopologyDigest = workspace.Topology.Digest
	}
	if workspace.RemoteHost != "" {
		remoteCtx, remoteCancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = d.remotes.Call(remoteCtx, workspace.RemoteHost, "update_manifest", request)
		remoteCancel()
	} else {
		_, err = d.updateManifest(request)
	}
	if err != nil {
		return fmt.Errorf("commit pane admission: %w", err)
	}
	return nil
}

// recordAdmissionObservation folds one successful Kitty listing into each
// proposed pane's evidence and reports whether at least one is now tagged and
// therefore worth capturing for.
func (d *Daemon) recordAdmissionObservation(workspaceID, endpoint string, tagged map[string]bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return false
	}
	now := time.Now().UTC()
	admissible, changed := false, false
	for _, pane := range workspace.Panes {
		if !pane.Proposed() || pane.Admission.Endpoint != endpoint {
			continue
		}
		if tagged[pane.ID] {
			admissible = true
			if !pane.Admission.MissingSince.IsZero() {
				pane.Admission.MissingSince = time.Time{}
				changed = true
			}
			continue
		}
		if pane.Admission.MissingSince.IsZero() {
			pane.Admission.MissingSince = now
			changed = true
		}
	}
	if changed {
		_ = d.store.Save(d.state)
	}
	return admissible
}

// retirableProposedPane reports whether a proposed pane has run out of chances
// to prove it exists. A pane whose Kitty window is present is never retired,
// however old it is: retiring a live window's pane would leave Kitty showing a
// window tagged for an unknown pane, which wedges capture permanently.
func retirableProposedPane(pane *Pane, hasLiveAttachment bool, now time.Time) bool {
	if !pane.Proposed() {
		return false
	}
	startedAt := pane.PhaseAt
	if startedAt.IsZero() {
		startedAt = pane.UpdatedAt
	}
	if !pane.Admission.MissingSince.IsZero() {
		return now.Sub(pane.Admission.MissingSince) >= paneAdmissionGrace
	}
	if pane.Admission.Endpoint == "" || !hasLiveAttachment {
		return !startedAt.IsZero() && now.Sub(startedAt) >= paneAdmissionDeadline
	}
	return false
}

func (d *Daemon) endpointHasLiveAttachmentLocked(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	for _, workspace := range d.state.Workspaces {
		for _, attachment := range workspace.Attachments {
			if attachment.Endpoint == endpoint && attachment.Status != AttachmentDetached && !attachment.Revoked {
				return true
			}
		}
	}
	return false
}

// admitPane is the RPC `zka pane` calls once it has tagged its window and its
// client is ready. It is advisory: the background worker reaches the same
// state, so a killed CLI still converges.
func (d *Daemon) admitPane(ctx context.Context, req admitPaneRequest) (*Workspace, error) {
	workspace, err := d.getWorkspace(req.Workspace)
	if err != nil {
		return nil, err
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		if pane := workspace.Panes[req.Pane]; pane != nil {
			endpoint = pane.Admission.Endpoint
		}
	}
	if endpoint == "" {
		return workspace, nil
	}
	if err := d.admitPendingPanes(ctx, endpoint); err != nil {
		return nil, err
	}
	return d.getWorkspace(req.Workspace)
}
