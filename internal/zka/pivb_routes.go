package zka

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"sync"
	"time"
)

type pivbEndpointResponse struct {
	WorkspaceID string `json:"workspace_id"`
	Socket      string `json:"socket"`
	State       string `json:"state"`
	Source      string `json:"source,omitempty"`
	Bundle      string `json:"bundle,omitempty"`
	Generation  uint64 `json:"generation,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

type localPIVBListener struct {
	workspace string
	binding   WorkspacePIVBProvider
	path      string
	listener  net.Listener
	boundInfo os.FileInfo
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	active    map[net.Conn]context.CancelFunc
}

func (d *Daemon) activateLocalPIVB(ctx context.Context, workspaceRef, bundleName string, ifUnclaimed bool) (workspaceCredentialStatus, error) {
	bundle, ok := d.config.credentialBundle(bundleName)
	if !ok || !bundle.PIVB.Enable {
		return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q does not enable PIVB", bundleName)
	}
	d.mu.Lock()
	initial, err := d.resolveWorkspaceLocked(workspaceRef)
	d.mu.Unlock()
	if err != nil {
		return workspaceCredentialStatus{}, err
	}
	claimLock := d.credentialClaimLock(initial.ID)
	claimLock.Lock()
	defer claimLock.Unlock()

	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(initial.ID)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	if ifUnclaimed && workspace.PIVBProvider != nil {
		local := workspace.PIVBProvider.Source == "local"
		workspaceID := workspace.ID
		d.mu.Unlock()
		if local {
			d.reconcileLocalPIVBListeners()
		}
		status, err := d.workspaceCredentialStatus(workspaceID)
		if err != nil {
			return status, err
		}
		if local {
			if capability := status.Capabilities[credentialCapabilityPIVB]; !capability.Available {
				return status, fmt.Errorf("local PIVB route is %s: %s", capability.State, capability.Detail)
			}
		}
		return status, nil
	}
	if workspace.PIVBProvider != nil && workspace.PIVBProvider.Source == "attachment" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace %q PIVB is provided by attachment %s; local activation will not replace it", workspace.Name, workspace.PIVBProvider.OwnerAttachmentID)
	}
	workspaceID := workspace.ID
	previousGeneration := uint64(0)
	if workspace.PIVBProvider != nil {
		previousGeneration = workspace.PIVBProvider.Generation
	}
	d.mu.Unlock()

	manifest, err := buildPIVBManifest(ctx, d.config, bundle.PIVB.Aliases)
	if err != nil {
		return workspaceCredentialStatus{}, err
	}
	now := time.Now().UTC()
	d.mu.Lock()
	workspace, err = d.resolveWorkspaceLocked(workspaceID)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if workspace.PIVBProvider != nil && workspace.PIVBProvider.Source == "attachment" {
		if ifUnclaimed {
			d.mu.Unlock()
			return d.workspaceCredentialStatus(workspaceID)
		}
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace PIVB provider changed while preparing local activation")
	}
	if workspace.PIVBProvider != nil && workspace.PIVBProvider.Source == "local" && workspace.PIVBProvider.Bundle == bundleName && reflect.DeepEqual(workspace.PIVBProvider.Manifest, *manifest) && workspace.PIVBProvider.State == "ready" {
		d.mu.Unlock()
		return d.workspaceCredentialStatus(workspaceID)
	}
	previous := workspace.PIVBProvider
	previousUpdatedAt := workspace.UpdatedAt
	workspace.PIVBProvider = &WorkspacePIVBProvider{
		Source: "local", Bundle: bundleName, Generation: previousGeneration + 1,
		OwnerNodeID: d.state.Node.ID, Manifest: *manifest, State: "starting", UpdatedAt: now,
	}
	workspace.UpdatedAt = now
	if err := d.store.Save(d.state); err != nil {
		workspace.PIVBProvider = previous
		workspace.UpdatedAt = previousUpdatedAt
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	d.mu.Unlock()
	d.reconcileLocalPIVBListeners()
	status, err := d.workspaceCredentialStatus(workspaceID)
	if err != nil {
		return status, err
	}
	if capability := status.Capabilities[credentialCapabilityPIVB]; !capability.Available {
		return status, fmt.Errorf("local PIVB route is %s: %s", capability.State, capability.Detail)
	}
	return status, nil
}

func (d *Daemon) pivbEndpoint(workspaceRef string) (pivbEndpointResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return pivbEndpointResponse{}, err
	}
	if workspace.RemoteHost != "" {
		return pivbEndpointResponse{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	result := pivbEndpointResponse{WorkspaceID: workspace.ID, Socket: pivbRelaySocketPath(d.paths, workspace.ID), State: "unclaimed"}
	if provider := workspace.PIVBProvider; provider != nil {
		result.State, result.Source, result.Bundle, result.Generation = provider.State, provider.Source, provider.Bundle, provider.Generation
		result.Detail = provider.LastError
		if result.State == "ready" && !credentialSocketPublished(result.Socket) {
			result.State = "degraded"
			result.Detail = appendCredentialDetail(result.Detail, "workspace PIVB route listener is not published")
		}
	}
	return result, nil
}

func (d *Daemon) localPIVBReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer d.closeAllLocalPIVBListeners()
	for {
		d.reconcileLocalPIVBListeners()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) reconcileLocalPIVBListeners() {
	d.pivbReconcileMu.Lock()
	defer d.pivbReconcileMu.Unlock()
	d.mu.Lock()
	desired := map[string]WorkspacePIVBProvider{}
	for _, workspace := range d.state.Workspaces {
		if workspace.RemoteHost == "" && workspace.PIVBProvider != nil && workspace.PIVBProvider.Source == "local" {
			desired[workspace.ID] = *workspace.PIVBProvider
		}
	}
	d.mu.Unlock()

	var closeListeners []*localPIVBListener
	ready := map[string]uint64{}
	d.credentialMu.Lock()
	for workspace, listener := range d.pivbLocalListeners {
		binding, ok := desired[workspace]
		if !ok || binding.Generation != listener.binding.Generation || !listener.socketPublished() {
			delete(d.pivbLocalListeners, workspace)
			closeListeners = append(closeListeners, listener)
			continue
		}
		ready[workspace] = binding.Generation
		delete(desired, workspace)
	}
	d.credentialMu.Unlock()
	for _, listener := range closeListeners {
		listener.close()
	}
	for workspace, generation := range ready {
		d.setLocalPIVBObservedState(workspace, generation, "ready", "")
	}
	for workspace, binding := range desired {
		listener, err := d.startLocalPIVBListener(workspace, binding)
		if err != nil {
			d.logger.Printf("local PIVB route %s generation %d failed to start: %v", workspace, binding.Generation, err)
			d.setLocalPIVBObservedState(workspace, binding.Generation, "degraded", err.Error())
			continue
		}
		if !d.localPIVBBindingOwned(workspace, binding.Generation) {
			listener.close()
			continue
		}
		d.credentialMu.Lock()
		if d.pivbLocalListeners[workspace] == nil {
			d.pivbLocalListeners[workspace] = listener
			d.credentialMu.Unlock()
			d.setLocalPIVBObservedState(workspace, binding.Generation, "ready", "")
			continue
		}
		d.credentialMu.Unlock()
		listener.close()
	}
}

func (d *Daemon) setLocalPIVBObservedState(workspaceID string, generation uint64, state, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.PIVBProvider == nil || workspace.PIVBProvider.Source != "local" || workspace.PIVBProvider.Generation != generation ||
		workspace.PIVBProvider.State == state && workspace.PIVBProvider.LastError == detail {
		return
	}
	previous := *workspace.PIVBProvider
	previousUpdated := workspace.UpdatedAt
	workspace.PIVBProvider.State = state
	workspace.PIVBProvider.LastError = detail
	workspace.PIVBProvider.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = workspace.PIVBProvider.UpdatedAt
	if err := d.store.Save(d.state); err != nil {
		*workspace.PIVBProvider = previous
		workspace.UpdatedAt = previousUpdated
		d.logger.Printf("persist local PIVB route %s observed state: %v", workspaceID, err)
	}
}

func (d *Daemon) localPIVBBindingCurrent(workspaceID string, generation uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	return workspace != nil && workspace.RemoteHost == "" && workspace.PIVBProvider != nil &&
		workspace.PIVBProvider.Source == "local" && workspace.PIVBProvider.Generation == generation && workspace.PIVBProvider.State == "ready"
}

func (d *Daemon) localPIVBBindingOwned(workspaceID string, generation uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	return workspace != nil && workspace.RemoteHost == "" && workspace.PIVBProvider != nil &&
		workspace.PIVBProvider.Source == "local" && workspace.PIVBProvider.Generation == generation
}

func (d *Daemon) closeLocalPIVBListener(workspaceID string) {
	d.credentialMu.Lock()
	listener := d.pivbLocalListeners[workspaceID]
	delete(d.pivbLocalListeners, workspaceID)
	d.credentialMu.Unlock()
	if listener != nil {
		listener.close()
	}
}

func (d *Daemon) startLocalPIVBListener(workspace string, binding WorkspacePIVBProvider) (*localPIVBListener, error) {
	path := pivbRelaySocketPath(d.paths, workspace)
	ln, err := listenUnix(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	listener := &localPIVBListener{workspace: workspace, binding: binding, path: path, listener: ln, boundInfo: info, done: make(chan struct{}), active: map[net.Conn]context.CancelFunc{}}
	d.startWorker(func(workerCtx context.Context) { listener.serve(workerCtx, d) })
	return listener, nil
}

func (l *localPIVBListener) serve(ctx context.Context, d *Daemon) {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			return
		}
		operationCtx, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		l.active[conn] = cancel
		l.mu.Unlock()
		go func() {
			defer func() {
				cancel()
				_ = conn.Close()
				l.mu.Lock()
				delete(l.active, conn)
				l.mu.Unlock()
			}()
			if !d.localPIVBBindingCurrent(l.workspace, l.binding.Generation) {
				_ = writePIVBProxyError(conn, 503, "PIVB_UNAVAILABLE", "local PIVB route has been replaced")
				return
			}
			_ = d.proxyPIVBMint(operationCtx, conn, l.workspace, l.binding.Bundle, l.binding.Generation, "", l.binding.OwnerNodeID, &l.binding.Manifest)
		}()
	}
}

func (l *localPIVBListener) socketPublished() bool {
	current, err := os.Lstat(l.path)
	return err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(l.boundInfo, current)
}

func (l *localPIVBListener) close() {
	l.once.Do(func() {
		close(l.done)
		_ = l.listener.Close()
		l.mu.Lock()
		for conn, cancel := range l.active {
			cancel()
			_ = conn.Close()
		}
		l.mu.Unlock()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.boundInfo, current) {
			_ = os.Remove(l.path)
		}
	})
}

func (d *Daemon) closeAllLocalPIVBListeners() {
	d.credentialMu.Lock()
	listeners := make([]*localPIVBListener, 0, len(d.pivbLocalListeners))
	for _, listener := range d.pivbLocalListeners {
		listeners = append(listeners, listener)
	}
	d.pivbLocalListeners = map[string]*localPIVBListener{}
	d.credentialMu.Unlock()
	for _, listener := range listeners {
		listener.close()
	}
}
