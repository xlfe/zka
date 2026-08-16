package zka

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	credentialMigrationPending  = "pending"
	credentialMigrationStarting = "starting"
	credentialMigrationFailed   = "failed"
)

type credentialMigrationCandidate struct {
	workspaceID string
	shell       []string
	pane        *Pane
}

// migrateCredentialEnvironments performs the guarded recreation work. It is
// invoked only by the explicit recreate API (and focused tests), never by a
// background reconciliation loop.
func (d *Daemon) migrateCredentialEnvironments(ctx context.Context) {
	if !d.config.credentialsEnabled() {
		return
	}
	d.credentialMigrationMu.Lock()
	defer d.credentialMigrationMu.Unlock()

	workspaceIDs := d.workspacesRequiringCredentialMigration()
	if len(workspaceIDs) == 0 {
		return
	}
	readyWorkspaces := make(map[string]bool, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		if err := d.ensureCredentialMigrationBinding(ctx, workspaceID); err != nil {
			d.recordWorkspaceCredentialMigrationError(workspaceID, err)
			continue
		}
		readyWorkspaces[workspaceID] = true
	}

	allCandidates := d.credentialMigrationCandidates()
	candidates := make([]credentialMigrationCandidate, 0, len(allCandidates))
	for _, candidate := range allCandidates {
		if readyWorkspaces[candidate.workspaceID] {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return
	}
	if err := d.checkDetachedZMXRun(ctx); err != nil {
		for _, candidate := range candidates {
			d.recordPaneCredentialMigrationError(candidate.workspaceID, candidate.pane.ID, err, false)
		}
		return
	}
	d.reconcileCredentialRoutes(ctx)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		if err := d.validateCredentialMigrationRoutes(candidate.workspaceID); err != nil {
			d.recordPaneCredentialMigrationError(candidate.workspaceID, candidate.pane.ID, err, false)
			continue
		}
		if backendStopped, err := d.migrateCredentialPane(ctx, candidate); err != nil {
			d.recordPaneCredentialMigrationError(candidate.workspaceID, candidate.pane.ID, err, backendStopped)
		}
	}
}

func (d *Daemon) recreateCredentialBackends(ctx context.Context, workspaceRef string) (*Workspace, error) {
	d.credentialMigrationMu.Lock()
	defer d.credentialMigrationMu.Unlock()
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return nil, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	workspaceID := workspace.ID
	d.mu.Unlock()
	if err := d.ensureCredentialMigrationBinding(ctx, workspaceID); err != nil {
		return nil, err
	}
	if err := ensureManagedPIVBLaunchCapabilities(ctx, d.config, d.runner); err != nil {
		return nil, err
	}
	if err := d.checkDetachedZMXRun(ctx); err != nil {
		return nil, err
	}
	d.reconcileCredentialRoutes(ctx)
	for _, candidate := range d.credentialMigrationCandidates() {
		if candidate.workspaceID != workspaceID {
			continue
		}
		if err := d.validateCredentialMigrationRoutes(workspaceID); err != nil {
			return nil, err
		}
		backendStopped, err := d.migrateCredentialPane(ctx, candidate)
		if err != nil {
			d.recordPaneCredentialMigrationError(candidate.workspaceID, candidate.pane.ID, err, backendStopped)
			return nil, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace = d.state.Workspaces[workspaceID]
	if workspace == nil {
		return nil, fmt.Errorf("workspace disappeared during backend recreation")
	}
	return workspace.Clone(), nil
}

func (d *Daemon) validateCredentialMigrationRoutes(workspaceID string) error {
	d.mu.Lock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.CredentialClaim == nil || workspace.CredentialClaim.State != "ready" {
		d.mu.Unlock()
		return fmt.Errorf("backend recreation requires a ready credential binding")
	}
	capabilities := cloneCredentialCapabilities(workspace.CredentialClaim.Capabilities)
	d.mu.Unlock()
	if err := d.validateCredentialRoutes(workspaceID, capabilities); err != nil {
		return fmt.Errorf("backend recreation is waiting for stable credential routes: %w", err)
	}
	return nil
}

func (d *Daemon) workspacesRequiringCredentialMigration() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, workspace := range d.state.Workspaces {
		if workspace.RemoteHost == "" && !workspace.DeletionPending && len(panesRequiringCredentialEnvironmentVersion(workspace, credentialEnvironmentVersionForConfig(d.config))) != 0 {
			ids = append(ids, workspace.ID)
		}
	}
	return sortedEndpointSet(ids)
}

func (d *Daemon) ensureCredentialMigrationBinding(ctx context.Context, workspaceID string) error {
	d.mu.Lock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.RemoteHost != "" || workspace.DeletionPending {
		d.mu.Unlock()
		return nil
	}
	if workspace.CredentialClaim != nil {
		claimSource := workspace.CredentialClaim.ProviderSource
		claimBundle := workspace.CredentialClaim.Bundle
		ownerAttachment := workspace.CredentialClaim.OwnerAttachmentID
		d.mu.Unlock()
		if claimSource == "local" {
			migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if _, err := d.activateLocalCredentialBundle(migrationCtx, workspaceID, claimBundle, false, "", ownerAttachment, 0); err != nil {
				return fmt.Errorf("refresh local credential binding before migration: %w", err)
			}
		}
		var status *workspaceCredentialStatus
		statuses := d.allCredentialStatuses()
		for index := range statuses.Workspaces {
			candidate := statuses.Workspaces[index]
			if candidate.WorkspaceID == workspaceID {
				status = &candidate
				break
			}
		}
		if status == nil {
			return fmt.Errorf("backend recreation workspace disappeared")
		}
		if status.State != "ready" {
			return fmt.Errorf("backend recreation is waiting for credential binding generation %d to become ready", status.Generation)
		}
		return nil
	}
	d.mu.Unlock()
	return fmt.Errorf("managed backend recreation requires an explicit attachment-backed credential claim")
}

func (d *Daemon) credentialMigrationCandidates() []credentialMigrationCandidate {
	d.mu.Lock()
	defer d.mu.Unlock()
	var candidates []credentialMigrationCandidate
	for _, workspace := range d.state.Workspaces {
		if workspace.RemoteHost != "" || workspace.DeletionPending || workspace.CredentialClaim == nil {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane.BackendDead || pane.Retiring() || pane.CredentialEnvironmentVersion == credentialEnvironmentVersionForConfig(d.config) {
				continue
			}
			if !pane.BackendCreated && pane.CredentialMigrationState != credentialMigrationStarting && pane.CredentialMigrationState != credentialMigrationPending {
				continue
			}
			if pane.CredentialMigrationState == credentialMigrationStarting && time.Since(pane.UpdatedAt) < backendStartupGrace {
				continue
			}
			candidates = append(candidates, credentialMigrationCandidate{
				workspaceID: workspace.ID,
				shell:       append([]string(nil), workspace.Shell...), pane: pane.Clone(),
			})
		}
	}
	return candidates
}

func (d *Daemon) checkDetachedZMXRun(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stdout, stderr, err := d.runner.Run(callCtx, d.config.ZMX.Command, "help")
	if err != nil {
		return fmt.Errorf("verify detached zmx migration support: %w", err)
	}
	help := strings.ToLower(stdout + "\n" + stderr)
	if (!strings.Contains(help, "run <name>") && !strings.Contains(help, "[r]un <name>")) || !strings.Contains(help, "[-d]") {
		return fmt.Errorf("zmx does not advertise `run <name> [-d]`; recreate this workspace after upgrading zmx")
	}
	return nil
}

func (d *Daemon) migrateCredentialPane(ctx context.Context, candidate credentialMigrationCandidate) (bool, error) {
	active, err := listZMXSessions(ctx, d.runner, d.config.ZMX.Command)
	if err != nil {
		return false, fmt.Errorf("list zmx sessions before migrating pane %s: %w", candidate.pane.ID, err)
	}
	backendStopped := !active[candidate.pane.Backend.Ref]
	if err := d.markPaneCredentialMigration(candidate.workspaceID, candidate.pane.ID, credentialMigrationPending); err != nil {
		return backendStopped, err
	}
	if active[candidate.pane.Backend.Ref] {
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, stderr, killErr := d.runner.Run(callCtx, d.config.ZMX.Command, "kill", candidate.pane.Backend.Ref, "--force")
		cancel()
		if killErr != nil {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = killErr.Error()
			}
			return false, fmt.Errorf("stop route-unsafe zmx backend %s: %s", candidate.pane.Backend.Ref, detail)
		}
		backendStopped = true
		active, err = listZMXSessions(ctx, d.runner, d.config.ZMX.Command)
		if err != nil {
			return backendStopped, fmt.Errorf("confirm route-unsafe zmx backend stopped: %w", err)
		}
		if active[candidate.pane.Backend.Ref] {
			return false, fmt.Errorf("route-unsafe zmx backend %s is still running", candidate.pane.Backend.Ref)
		}
	}

	if err := d.preparePaneCredentialMigrationRestart(candidate.workspaceID, candidate.pane.ID); err != nil {
		return backendStopped, err
	}
	environment := managedPaneCommandEnvironment(d.config, d.paths, candidate.workspaceID, candidate.pane.ID, true)
	args := []string{"run", candidate.pane.Backend.Ref, "-d", "zka", "pane-host", "--workspace", candidate.workspaceID, "--pane", candidate.pane.ID, "--"}
	args = append(args, candidate.shell...)
	directory := ""
	if usableDirectory(candidate.pane.CWD) {
		directory = candidate.pane.CWD
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, stderr, runErr := runConfiguredCommand(callCtx, d.runner, d.config.ZMX.Command, args, environment, directory)
	cancel()
	if runErr != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = runErr.Error()
		}
		return true, fmt.Errorf("start managed replacement for pane %s: %s", candidate.pane.ID, detail)
	}
	return true, nil
}

func (d *Daemon) markPaneCredentialMigration(workspaceID, paneID, state string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return fmt.Errorf("credential migration pane disappeared")
	}
	pane := workspace.Panes[paneID]
	pane.CredentialMigrationState = state
	pane.CredentialMigrationError = ""
	pane.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = pane.UpdatedAt
	return d.store.Save(d.state)
}

func (d *Daemon) preparePaneCredentialMigrationRestart(workspaceID, paneID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return fmt.Errorf("credential migration pane disappeared")
	}
	pane := workspace.Panes[paneID]
	pane.BackendCreated, pane.BackendReady, pane.BackendDead = false, false, false
	pane.BackendStart = true
	pane.BackendError = ""
	pane.Process = ProcessStatus{}
	pane.CredentialMigrationState = credentialMigrationStarting
	pane.CredentialMigrationError = ""
	pane.Evidence = Evidence{Source: "zkad", Event: "credential_environment_migration", Detail: "restarting zmx backend with stable managed credential endpoints", Timestamp: time.Now().UTC()}
	pane.UpdatedAt = pane.Evidence.Timestamp
	workspace.UpdatedAt = pane.UpdatedAt
	return d.store.Save(d.state)
}

func (d *Daemon) recordWorkspaceCredentialMigrationError(workspaceID string, err error) {
	d.mu.Lock()
	workspace := d.state.Workspaces[workspaceID]
	var panes []string
	if workspace != nil {
		panes = panesRequiringCredentialEnvironmentVersion(workspace, credentialEnvironmentVersionForConfig(d.config))
	}
	d.mu.Unlock()
	for _, paneID := range panes {
		d.recordPaneCredentialMigrationError(workspaceID, paneID, err, false)
	}
}

func (d *Daemon) recordPaneCredentialMigrationError(workspaceID, paneID string, migrationErr error, backendStopped bool) {
	if migrationErr == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return
	}
	pane := workspace.Panes[paneID]
	detail := migrationErr.Error()
	if pane.CredentialMigrationState == credentialMigrationFailed && pane.CredentialMigrationError == detail {
		return
	}
	pane.CredentialMigrationState = credentialMigrationFailed
	pane.CredentialMigrationError = detail
	if backendStopped {
		// Once the old backend is gone, failure to start its managed replacement
		// is an incomplete migration, not ordinary backend death. Keep it in the
		// reaper's protected set and retry on the next migration pass.
		pane.CredentialMigrationState = credentialMigrationPending
		pane.BackendCreated, pane.BackendReady, pane.BackendStart, pane.BackendDead = false, false, true, false
		pane.BackendError = detail
		pane.Process = ProcessStatus{}
		pane.State = StateError
	} else if !pane.BackendCreated {
		pane.BackendCreated, pane.BackendReady, pane.BackendStart, pane.BackendDead = false, false, false, true
		pane.BackendError = detail
		pane.Process = ProcessStatus{}
		pane.State = StateError
	}
	pane.Evidence = Evidence{Source: "zkad", Event: "credential_environment_migration_failed", Detail: detail, Timestamp: time.Now().UTC()}
	pane.UpdatedAt = pane.Evidence.Timestamp
	workspace.UpdatedAt = pane.UpdatedAt
	workspace.RecomputeAttention()
	if err := d.store.Save(d.state); err != nil {
		d.logger.Printf("persist credential migration failure workspace=%s pane=%s: %v", workspaceID, paneID, err)
		return
	}
	d.logger.Printf("credential migration blocked workspace=%s pane=%s: %s", workspaceID, paneID, detail)
}
