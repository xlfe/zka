package zka

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const backendStartupGrace = 30 * time.Second

func requireWorkspaceMutable(workspace *Workspace) error {
	if workspace.DeletionPending {
		return fmt.Errorf("workspace %q is being deleted", workspace.Name)
	}
	return nil
}

func (d *Daemon) renameWorkspace(req renameWorkspaceRequest) (*Workspace, error) {
	name := strings.TrimSpace(req.Name)
	if err := validateName(name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	if workspace.RemoteHost != "" {
		return nil, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	if workspace.Name == name {
		return workspace.Clone(), nil
	}
	if req.ExpectedRevision != 0 && req.ExpectedRevision != workspace.Revision {
		return nil, fmt.Errorf("workspace revision changed: have %d, expected %d", workspace.Revision, req.ExpectedRevision)
	}
	for _, existing := range d.state.Workspaces {
		if existing.ID != workspace.ID && existing.RemoteHost == "" && existing.Name == name {
			return nil, fmt.Errorf("workspace name %q already exists", name)
		}
	}
	before := workspace.Clone()
	previousRevision := workspace.Revision
	workspace.Name = name
	workspace.Revision++
	workspace.UpdatedAt = time.Now().UTC()
	for _, attachment := range workspace.Attachments {
		if attachment.AppliedRevision == previousRevision {
			attachment.AppliedRevision = workspace.Revision
		}
	}
	if err := d.store.Save(d.state); err != nil {
		d.state.Workspaces[workspace.ID] = before
		return nil, err
	}
	return workspace.Clone(), nil
}

func (d *Daemon) closePanes(ctx context.Context, req closePanesRequest) (*Workspace, error) {
	workspace, pending, err := d.beginPaneClosure(req)
	if err != nil || !pending {
		return workspace, err
	}
	complete := d.cleanupWorkspaceOnce(ctx, workspace.ID)
	if !complete {
		d.scheduleLifecycleCleanup(workspace.ID)
	}
	if current, getErr := d.getWorkspace(workspace.ID); getErr == nil {
		return current, nil
	}
	return workspace, nil
}

func (d *Daemon) beginPaneClosure(req closePanesRequest) (*Workspace, bool, error) {
	if len(req.Panes) == 0 {
		return nil, false, fmt.Errorf("close panes requires at least one pane")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return nil, false, err
	}
	if workspace.RemoteHost != "" {
		return nil, false, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	if workspace.DeletionPending {
		return workspace.Clone(), true, nil
	}
	before := workspace.Clone()

	requested := map[string]bool{}
	allAlreadyRemoved := true
	allAlreadyScheduled := true
	for _, paneID := range req.Panes {
		if paneID == "" || requested[paneID] {
			continue
		}
		requested[paneID] = true
		if pane := workspace.Panes[paneID]; pane != nil {
			allAlreadyRemoved = false
			if !pane.RemovalPending {
				allAlreadyScheduled = false
			}
		}
	}
	if len(requested) == 0 {
		return nil, false, fmt.Errorf("close panes requires valid pane ids")
	}
	if allAlreadyRemoved {
		return workspace.Clone(), false, nil
	}
	if allAlreadyScheduled {
		return workspace.Clone(), true, nil
	}
	if req.ExpectedRevision == 0 || req.ExpectedRevision != workspace.Revision {
		return nil, false, fmt.Errorf("workspace revision changed: have %d, expected %d", workspace.Revision, req.ExpectedRevision)
	}
	attachment := workspace.Attachments[req.Attachment]
	if attachment == nil {
		return nil, false, fmt.Errorf("unknown attachment %q", req.Attachment)
	}
	if attachment.Status != AttachmentReady || attachment.Revoked || attachment.AppliedRevision != workspace.Revision {
		return nil, false, fmt.Errorf("attachment %s is not a current ready attachment", attachment.ID)
	}
	for paneID := range requested {
		pane := workspace.Panes[paneID]
		if pane == nil {
			return nil, false, fmt.Errorf("unknown pane %q", paneID)
		}
		if _, owned := attachment.Views[paneID]; !owned {
			return nil, false, fmt.Errorf("attachment %s did not own pane %s before it closed", attachment.ID, paneID)
		}
		if _, stillVisible := req.Views[paneID]; stillVisible {
			return nil, false, fmt.Errorf("closed pane %s is still present in the captured views", paneID)
		}
	}

	remaining := map[string]bool{}
	for paneID, pane := range workspace.Panes {
		if !requested[paneID] && !pane.RemovalPending {
			remaining[paneID] = true
		}
	}
	now := time.Now().UTC()
	if len(remaining) == 0 {
		workspace.DeletionPending = true
		workspace.DeletionError = ""
		workspace.Revision++
		workspace.UpdatedAt = now
		if err := d.store.Save(d.state); err != nil {
			d.state.Workspaces[workspace.ID] = before
			return nil, false, err
		}
		return workspace.Clone(), true, nil
	}
	if err := validateManifest(workspace, req.Manifest); err != nil {
		return nil, false, err
	}
	captured := topologyPaneIDs(req.Manifest.Topology)
	if !samePaneSet(captured, remaining) {
		return nil, false, fmt.Errorf("captured topology does not equal the workspace after the requested pane closure")
	}
	temporary := attachment.Clone()
	temporary.Views = req.Views
	if err := validateViewsReady(temporary, remaining); err != nil {
		return nil, false, err
	}
	if attachment.Transport.Kind == "ssh" {
		preserveOriginPaneCWD(workspace, req.Manifest.Topology)
	}
	applyManifestPaneMetadata(workspace, req.Manifest.Topology, attachment.Transport.Kind != "ssh")
	for paneID := range requested {
		pane := workspace.Panes[paneID]
		pane.Visible = false
		pane.RemovalPending = true
		pane.RemovalError = ""
		pane.UpdatedAt = now
	}
	req.Manifest.CapturedAt = now
	workspace.Manifest = req.Manifest
	workspace.Revision++
	workspace.UpdatedAt = now
	attachment.Views = cloneViews(req.Views)
	attachment.AppliedRevision = workspace.Revision
	attachment.LastError = ""
	attachment.UpdatedAt = now
	if err := d.store.Save(d.state); err != nil {
		d.state.Workspaces[workspace.ID] = before
		return nil, false, err
	}
	return workspace.Clone(), true, nil
}

func samePaneSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if !right[id] {
			return false
		}
	}
	return true
}

func (d *Daemon) killWorkspace(ctx context.Context, workspaceID string) (workspaceDeletionResponse, error) {
	response, pending, err := d.beginWorkspaceDeletion(workspaceID)
	if err != nil || !pending {
		return response, err
	}
	for {
		if d.cleanupWorkspaceOnce(ctx, workspaceID) {
			response.Remaining = nil
			return response, nil
		}
		select {
		case <-ctx.Done():
			response.Remaining = d.pendingBackendNames(workspaceID)
			d.scheduleLifecycleCleanup(workspaceID)
			if len(response.Remaining) != 0 {
				return response, fmt.Errorf("workspace %q is still being deleted; remaining zmx sessions: %s", response.Name, strings.Join(response.Remaining, ", "))
			}
			return response, fmt.Errorf("workspace %q cleanup is still pending after its zmx sessions were removed", response.Name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (d *Daemon) beginWorkspaceDeletion(workspaceID string) (workspaceDeletionResponse, bool, error) {
	if workspaceID == "" {
		return workspaceDeletionResponse{}, false, fmt.Errorf("workspace id is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if name, ok := d.deleted[workspaceID]; ok {
		return workspaceDeletionResponse{DeletedWorkspaceID: workspaceID, Name: name}, false, nil
	}
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return workspaceDeletionResponse{}, false, fmt.Errorf("unknown workspace %q", workspaceID)
	}
	if workspace.RemoteHost != "" {
		return workspaceDeletionResponse{}, false, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	response := workspaceDeletionResponse{DeletedWorkspaceID: workspace.ID, Name: workspace.Name}
	if workspace.DeletionPending {
		return response, true, nil
	}
	before := workspace.Clone()
	workspace.DeletionPending = true
	workspace.DeletionError = ""
	workspace.Revision++
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		d.state.Workspaces[workspaceID] = before
		return workspaceDeletionResponse{}, false, err
	}
	return response, true, nil
}

func (d *Daemon) cleanupWorkspaceOnce(ctx context.Context, workspaceID string) bool {
	cleanup := d.workspaceCleanupLock(workspaceID)
	cleanup.Lock()
	defer cleanup.Unlock()
	d.mu.Lock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		d.mu.Unlock()
		d.agentRelays.remove(workspaceID)
		return true
	}
	deleting := workspace.DeletionPending
	var targets []*Pane
	for _, pane := range workspace.Panes {
		if deleting || pane.RemovalPending {
			targets = append(targets, pane.Clone())
		}
	}
	var endpoints []string
	if deleting {
		for _, attachment := range workspace.Attachments {
			if attachment.Status != AttachmentDetached && attachment.Node.ID == d.state.Node.ID && strings.HasPrefix(attachment.Endpoint, "unix:") {
				endpoints = append(endpoints, attachment.Endpoint)
			}
		}
	}
	d.mu.Unlock()
	if !deleting && len(targets) == 0 {
		return true
	}

	var problems []string
	for _, endpoint := range endpoints {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.CloseWorkspace(callCtx, endpoint, workspaceID)
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("close Kitty at %s: %v", endpoint, err))
		}
	}
	active, err := listZMXSessions(ctx, d.runner, d.config.ZMX.Command)
	if err != nil {
		problems = append(problems, fmt.Sprintf("list zmx sessions: %v", err))
		d.recordLifecycleError(workspaceID, targets, strings.Join(problems, "; "))
		return false
	}
	for _, pane := range targets {
		if !active[pane.Backend.Ref] {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, stderr, killErr := d.runner.Run(callCtx, d.config.ZMX.Command, "kill", pane.Backend.Ref, "--force")
		cancel()
		if killErr != nil {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = killErr.Error()
			}
			problems = append(problems, fmt.Sprintf("kill %s: %s", pane.Backend.Ref, detail))
		}
	}
	active, err = listZMXSessions(ctx, d.runner, d.config.ZMX.Command)
	if err != nil {
		problems = append(problems, fmt.Sprintf("confirm zmx sessions: %v", err))
		d.recordLifecycleError(workspaceID, targets, strings.Join(problems, "; "))
		return false
	}
	remaining := map[string]bool{}
	for _, pane := range targets {
		if active[pane.Backend.Ref] {
			remaining[pane.ID] = true
			problems = append(problems, "zmx session still exists: "+pane.Backend.Ref)
		}
	}

	if deleting && len(remaining) == 0 {
		if err := d.store.RemoveWorkspaceSessions(workspaceID); err != nil {
			problems = append(problems, err.Error())
			d.recordLifecycleError(workspaceID, targets, strings.Join(problems, "; "))
			return false
		}
		d.mu.Lock()
		current := d.state.Workspaces[workspaceID]
		if current != nil && current.DeletionPending {
			name := current.Name
			delete(d.state.Workspaces, workspaceID)
			d.deleted[workspaceID] = name
			if err := d.store.Save(d.state); err != nil {
				d.state.Workspaces[workspaceID] = current
				delete(d.deleted, workspaceID)
				current.DeletionError = err.Error()
				d.mu.Unlock()
				return false
			}
		}
		d.mu.Unlock()
		d.agentRelays.remove(workspaceID)
		return true
	}
	if deleting {
		d.recordLifecycleError(workspaceID, targets, strings.Join(problems, "; "))
		return false
	}

	d.mu.Lock()
	current := d.state.Workspaces[workspaceID]
	if current == nil {
		d.mu.Unlock()
		return true
	}
	before := current.Clone()
	errorText := strings.Join(problems, "; ")
	var removedPaneIDs []string
	for _, pane := range targets {
		currentPane := current.Panes[pane.ID]
		if currentPane == nil || !currentPane.RemovalPending {
			continue
		}
		if remaining[pane.ID] {
			currentPane.RemovalError = errorText
			continue
		}
		delete(current.Panes, pane.ID)
		removedPaneIDs = append(removedPaneIDs, pane.ID)
		for _, attachment := range current.Attachments {
			delete(attachment.Views, pane.ID)
			delete(attachment.ClientHeartbeats, pane.ID)
		}
	}
	current.RecomputeAttention()
	current.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		d.state.Workspaces[workspaceID] = before
		d.mu.Unlock()
		return false
	}
	done := true
	for _, pane := range current.Panes {
		if pane.RemovalPending {
			done = false
			break
		}
	}
	d.mu.Unlock()
	for _, paneID := range removedPaneIDs {
		d.agentRelays.clearPane(workspaceID, paneID)
	}
	return done
}

func (d *Daemon) workspaceCleanupLock(workspaceID string) *sync.Mutex {
	d.cleanupMu.Lock()
	defer d.cleanupMu.Unlock()
	cleanup := d.cleanups[workspaceID]
	if cleanup == nil {
		cleanup = &sync.Mutex{}
		d.cleanups[workspaceID] = cleanup
	}
	return cleanup
}

func listZMXSessions(ctx context.Context, runner CommandRunner, command string) (map[string]bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _, err := runner.Run(callCtx, command, "list", "--short")
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 0 {
			result[fields[0]] = true
		}
	}
	return result, scanner.Err()
}

func (d *Daemon) backendReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.reconcileBackends(ctx, ""); err != nil && ctx.Err() == nil {
				d.logger.Printf("reconcile zmx backends: %v", err)
			}
		}
	}
}

func (d *Daemon) reconcileBackends(ctx context.Context, workspaceRef string) (backendReconcileResponse, error) {
	active, err := listZMXSessions(ctx, d.runner, d.config.ZMX.Command)
	if err != nil {
		return backendReconcileResponse{}, fmt.Errorf("list zmx sessions: %w", err)
	}

	d.mu.Lock()
	var workspaces []*Workspace
	if workspaceRef != "" {
		workspace, resolveErr := d.resolveWorkspaceLocked(workspaceRef)
		if resolveErr != nil {
			d.mu.Unlock()
			return backendReconcileResponse{}, resolveErr
		}
		if workspace.RemoteHost != "" {
			d.mu.Unlock()
			return backendReconcileResponse{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
		}
		workspaces = []*Workspace{workspace}
	} else {
		for _, workspace := range d.state.Workspaces {
			if workspace.RemoteHost == "" {
				workspaces = append(workspaces, workspace)
			}
		}
	}

	response := backendReconcileResponse{}
	var deleteIDs []string
	changed := false
	changedBefore := map[string]*Workspace{}
	now := time.Now().UTC()
	for _, workspace := range workspaces {
		if workspace.DeletionPending {
			continue
		}
		pending, live, established, removalPending := false, false, false, false
		workspaceChanged := false
		for _, pane := range workspace.Panes {
			if pane.RemovalPending {
				removalPending = true
				continue
			}
			if !pane.BackendCreated && !pane.BackendDead {
				startedAt := pane.UpdatedAt
				if startedAt.IsZero() {
					startedAt = pane.CreatedAt
				}
				if startedAt.IsZero() || now.Sub(startedAt) < backendStartupGrace {
					pending = true
					continue
				}
			}
			established = true
			if active[pane.Backend.Ref] {
				live = true
				if pane.BackendDead && pane.Evidence.Event == "backend_missing" {
					if !workspaceChanged {
						changedBefore[workspace.ID] = workspace.Clone()
					}
					pane.BackendCreated = true
					pane.BackendReady = true
					pane.BackendStart = false
					pane.BackendDead = false
					pane.BackendError = ""
					pane.Process.Running = true
					pane.Process.ExitCode = nil
					pane.Process.Exited = time.Time{}
					pane.State = StateUnknown
					pane.AttentionSeen = ""
					pane.Evidence = Evidence{
						Source:    "zkad",
						Event:     "backend_recovered",
						Detail:    fmt.Sprintf("zmx session %q is available", pane.Backend.Ref),
						Timestamp: now,
					}
					pane.UpdatedAt = now
					response.Recovered = append(response.Recovered, pane.ID)
					workspaceChanged = true
				}
				continue
			}
			if !pane.BackendDead || pane.BackendReady || pane.Process.Running || pane.State != StateError || pane.Evidence.Event != "backend_missing" {
				if !workspaceChanged {
					changedBefore[workspace.ID] = workspace.Clone()
				}
				pane.BackendDead = true
				pane.BackendReady = false
				pane.BackendStart = false
				pane.BackendError = fmt.Sprintf("zmx session %q is missing", pane.Backend.Ref)
				pane.Process.Running = false
				pane.Process.PID = 0
				pane.Process.Exited = now
				pane.State = StateError
				pane.AttentionSeen = ""
				pane.Evidence = Evidence{Source: "zkad", Event: "backend_missing", Detail: pane.BackendError, Timestamp: now}
				pane.UpdatedAt = now
				response.Marked = append(response.Marked, pane.ID)
				workspaceChanged = true
			}
		}
		if workspaceChanged {
			workspace.UpdatedAt = now
			workspace.RecomputeAttention()
			changed = true
		}
		if established && !pending && !live && !removalPending {
			deleteIDs = append(deleteIDs, workspace.ID)
		}
	}
	if changed {
		if err := d.store.Save(d.state); err != nil {
			for workspaceID, before := range changedBefore {
				d.state.Workspaces[workspaceID] = before
			}
			d.mu.Unlock()
			return backendReconcileResponse{}, err
		}
	}
	d.mu.Unlock()

	for _, workspaceID := range deleteIDs {
		if _, err := d.killWorkspace(ctx, workspaceID); err != nil {
			return response, err
		}
		response.Deleted = append(response.Deleted, workspaceID)
	}
	sort.Strings(response.Marked)
	sort.Strings(response.Deleted)
	return response, nil
}

func (d *Daemon) recordLifecycleError(workspaceID string, panes []*Pane, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		return
	}
	if workspace.DeletionPending {
		workspace.DeletionError = detail
	}
	for _, pane := range panes {
		if current := workspace.Panes[pane.ID]; current != nil && current.RemovalPending {
			current.RemovalError = detail
		}
	}
	workspace.UpdatedAt = time.Now().UTC()
	_ = d.store.Save(d.state)
}

func (d *Daemon) pendingBackendNames(workspaceID string) []string {
	d.mu.Lock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil {
		d.mu.Unlock()
		return nil
	}
	var candidates []string
	for _, pane := range workspace.Panes {
		if workspace.DeletionPending || pane.RemovalPending {
			candidates = append(candidates, pane.Backend.Ref)
		}
	}
	d.mu.Unlock()
	queryCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	active, err := listZMXSessions(queryCtx, d.runner, d.config.ZMX.Command)
	if err != nil {
		sort.Strings(candidates)
		return candidates
	}
	var names []string
	for _, name := range candidates {
		if active[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (d *Daemon) resumeLifecycleCleanup() {
	d.mu.Lock()
	var ids []string
	for id, workspace := range d.state.Workspaces {
		pending := workspace.DeletionPending
		for _, pane := range workspace.Panes {
			pending = pending || pane.RemovalPending
		}
		if pending && workspace.RemoteHost == "" {
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.scheduleLifecycleCleanup(id)
	}
}

func (d *Daemon) scheduleLifecycleCleanup(workspaceID string) {
	d.lifeMu.Lock()
	if d.cleaning[workspaceID] || d.closed {
		d.lifeMu.Unlock()
		return
	}
	d.cleaning[workspaceID] = true
	d.wg.Add(1)
	ctx := d.ctx
	d.lifeMu.Unlock()
	go func() {
		defer d.wg.Done()
		defer func() {
			d.lifeMu.Lock()
			delete(d.cleaning, workspaceID)
			d.lifeMu.Unlock()
		}()
		backoff := 250 * time.Millisecond
		for !d.cleanupWorkspaceOnce(ctx, workspaceID) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}()
}
