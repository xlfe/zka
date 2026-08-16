package zka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// credentialRouteListener is the pane-facing half of credential routing. It is
// owned by zkad and therefore survives provider changes and SSH control-session
// replacement. Provider transports never bind these paths.
type credentialRouteListener struct {
	workspace  string
	capability string
	path       string
	listener   net.Listener
	boundInfo  os.FileInfo
	once       sync.Once
	mu         sync.Mutex
	active     map[net.Conn]uint64
}

type desiredCredentialRoute struct {
	workspace  string
	capability string
	path       string
}

func credentialRouteKey(workspace, capability string) string {
	return capability + "\x00" + workspace
}

func credentialSocketPublished(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func (d *Daemon) credentialRouteLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer d.closeAllCredentialRoutes()
	for {
		d.reconcileCredentialRoutes(ctx)
		d.checkCredentialWindowNotices(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) reconcileCredentialRoutes(ctx context.Context) {
	sshEnabled, openPGPEnabled, pivbEnabled := false, false, false
	for _, bundle := range d.config.Credentials.Bundles {
		sshEnabled = sshEnabled || bundle.SSHAgent.Enable
		openPGPEnabled = openPGPEnabled || bundle.OpenPGP.Enable
		pivbEnabled = pivbEnabled || bundle.PIVB.Enable
	}

	d.mu.Lock()
	workspaceIDs := make([]string, 0, len(d.state.Workspaces))
	for _, workspace := range d.state.Workspaces {
		if workspace.RemoteHost == "" && !workspace.DeletionPending {
			workspaceIDs = append(workspaceIDs, workspace.ID)
		}
	}
	d.mu.Unlock()

	desired := map[string]desiredCredentialRoute{}
	for _, workspaceID := range workspaceIDs {
		if sshEnabled {
			route := desiredCredentialRoute{workspace: workspaceID, capability: credentialCapabilitySSH, path: agentRelaySocketPath(d.paths.AgentDir, workspaceID)}
			desired[credentialRouteKey(workspaceID, route.capability)] = route
		}
		if openPGPEnabled {
			path, err := d.credentialOpenPGPRoutePath(ctx, workspaceID)
			if err == nil {
				route := desiredCredentialRoute{workspace: workspaceID, capability: credentialCapabilityOpenPGP, path: path}
				desired[credentialRouteKey(workspaceID, route.capability)] = route
			}
		}
		if pivbEnabled {
			route := desiredCredentialRoute{workspace: workspaceID, capability: credentialCapabilityPIVB, path: pivbRelaySocketPath(d.paths, workspaceID)}
			desired[credentialRouteKey(workspaceID, route.capability)] = route
		}
	}

	d.credentialMu.Lock()
	var closing []*credentialRouteListener
	for key, listener := range d.credentialRoutes {
		route, ok := desired[key]
		if !ok || route.path != listener.path || !listener.socketPublished() {
			delete(d.credentialRoutes, key)
			closing = append(closing, listener)
			continue
		}
		delete(desired, key)
	}
	d.credentialMu.Unlock()
	for _, listener := range closing {
		listener.close()
	}

	for key, route := range desired {
		listener, err := d.startCredentialRoute(route)
		if err != nil {
			continue
		}
		d.credentialMu.Lock()
		if d.credentialRoutes[key] == nil {
			d.credentialRoutes[key] = listener
			d.credentialMu.Unlock()
			continue
		}
		d.credentialMu.Unlock()
		listener.close()
	}
}

func (d *Daemon) preparePaneCredentialRoutes(ctx context.Context, workspaceRef, paneRef string) error {
	if !d.config.credentialsEnabled() {
		return nil
	}
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	workspaceID := workspace.ID
	authoritative := workspace.RemoteHost == ""
	needsBackend := true
	unsafePaneID, unsafePaneVersion := "", 0
	if paneRef != "" {
		pane, paneErr := resolvePaneLocked(workspace, paneRef)
		if paneErr != nil {
			d.mu.Unlock()
			return paneErr
		}
		needsBackend = !pane.BackendCreated && !pane.BackendStart
		if configHasPIVBBundle(d.config) && pane.BackendCreated && !pane.BackendDead &&
			pane.CredentialEnvironmentVersion != credentialEnvironmentVersionForConfig(d.config) {
			unsafePaneID, unsafePaneVersion = pane.ID, pane.CredentialEnvironmentVersion
		}
	}
	d.mu.Unlock()
	if authoritative && unsafePaneID != "" {
		return fmt.Errorf("pane %s uses route-unsafe credential environment v%d; run `zka workspace reconcile --recreate-backends %s` before attaching", unsafePaneID, unsafePaneVersion, workspaceID)
	}
	if !authoritative || !needsBackend {
		return nil
	}
	capabilities := map[string]CredentialCapabilityStatus{}
	pivbEnabled := false
	for _, bundle := range d.config.Credentials.Bundles {
		if bundle.SSHAgent.Enable {
			capabilities[credentialCapabilitySSH] = CredentialCapabilityStatus{State: "ready", Available: true}
		}
		if bundle.OpenPGP.Enable {
			capabilities[credentialCapabilityOpenPGP] = CredentialCapabilityStatus{State: "ready", Available: true}
		}
		if bundle.PIVB.Enable {
			pivbEnabled = true
			capabilities[credentialCapabilityPIVB] = CredentialCapabilityStatus{State: "ready", Available: true}
		}
	}
	if len(capabilities) == 0 {
		return nil
	}
	// Probe before route publication or backend reservation. The daemon's
	// configured executable and the pane-visible PATH executable are checked
	// independently; either one being stale must fail before launch.
	if pivbEnabled {
		if err := ensureManagedPIVBLaunchCapabilities(ctx, d.config, d.runner); err != nil {
			return fmt.Errorf("prepare managed credential environment: %w", err)
		}
	}
	d.reconcileCredentialRoutes(ctx)
	if err := d.validateCredentialRoutes(workspaceID, capabilities); err != nil {
		return fmt.Errorf("prepare managed credential environment: %w", err)
	}
	return nil
}

func (d *Daemon) credentialOpenPGPRoutePath(ctx context.Context, workspaceID string) (string, error) {
	d.credentialMu.Lock()
	cached := d.credentialRoutePaths[workspaceID]
	d.credentialMu.Unlock()
	if cached != "" {
		return cached, nil
	}
	home, err := credentialOpenPGPHome(d.paths, workspaceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	providerRunner := d.providerRunner()
	if err := ensureGPGSocketDirectory(callCtx, d.config.Credentials.GnuPG.GPGConfCommand, home, providerRunner); err != nil {
		return "", err
	}
	path, _, err := providerRunner.Run(callCtx, d.config.Credentials.GnuPG.GPGConfCommand, "--homedir", home, "--list-dirs", "agent-socket")
	if err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("gpgconf returned invalid workspace agent socket %q", path)
	}
	d.credentialMu.Lock()
	d.credentialRoutePaths[workspaceID] = path
	d.credentialMu.Unlock()
	return path, nil
}

func (d *Daemon) startCredentialRoute(route desiredCredentialRoute) (*credentialRouteListener, error) {
	listener, err := listenUnix(route.path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(route.path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	result := &credentialRouteListener{
		workspace: route.workspace, capability: route.capability, path: route.path,
		listener: listener, boundInfo: info, active: map[net.Conn]uint64{},
	}
	d.startWorker(func(ctx context.Context) { result.serve(ctx, d) })
	return result, nil
}

func (l *credentialRouteListener) serve(_ context.Context, d *Daemon) {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			return
		}
		if !d.startWorker(func(workerCtx context.Context) { d.serveCredentialRoute(workerCtx, l, conn) }) {
			_ = conn.Close()
			return
		}
	}
}

func (d *Daemon) serveCredentialRoute(ctx context.Context, listener *credentialRouteListener, client net.Conn) {
	defer client.Close()
	d.mu.Lock()
	workspace := d.state.Workspaces[listener.workspace]
	if workspace == nil || workspace.RemoteHost != "" || workspace.CredentialClaim == nil {
		d.mu.Unlock()
		d.writeCredentialRouteUnavailable(client, listener.capability, "workspace credentials are unclaimed")
		return
	}
	if listener.capability == credentialCapabilityPIVB {
		required := credentialEnvironmentVersionForConfig(d.config)
		if unsafe := panesRequiringCredentialEnvironmentVersion(workspace, required); len(unsafe) != 0 {
			d.mu.Unlock()
			_ = writePIVBProxyError(client, http.StatusServiceUnavailable, "PIVB_UNAVAILABLE",
				fmt.Sprintf("workspace has route-unsafe pane backends %s; run `zka workspace reconcile --recreate-backends %s`", strings.Join(unsafe, ","), workspace.ID))
			return
		}
	}
	claim := *workspace.CredentialClaim
	claim.Capabilities = cloneCredentialCapabilities(workspace.CredentialClaim.Capabilities)
	claim.PIVB = clonePIVBManifest(workspace.CredentialClaim.PIVB)
	originNodeID := workspace.Origin.ID
	localNodeID := d.state.Node.ID
	capability, enabled := claim.Capabilities[listener.capability]
	if claim.State != "ready" || !enabled || !capability.Available {
		d.mu.Unlock()
		d.writeCredentialRouteUnavailable(client, listener.capability, "credential capability is unavailable")
		return
	}
	// Register while the claim snapshot is still protected by d.mu. A transfer
	// cannot commit between reading the old generation and publishing this
	// connection, so its subsequent revocation must see and close the stream.
	listener.mu.Lock()
	listener.active[client] = claim.Generation
	listener.mu.Unlock()
	d.mu.Unlock()
	defer func() {
		listener.mu.Lock()
		delete(listener.active, client)
		listener.mu.Unlock()
	}()

	hello := credentialStreamHello{Workspace: listener.workspace, Bundle: claim.Bundle, Capability: listener.capability, Generation: claim.Generation}
	if claim.ProviderSource == "local" || claim.ProviderSource == "" && claim.OwnerNodeID == localNodeID {
		d.serveLocalCredentialRoute(ctx, client, hello, &claim)
		return
	}
	d.serveRemoteCredentialRoute(ctx, client, hello, &claim, originNodeID)
}

func cloneCredentialCapabilities(source map[string]CredentialCapabilityStatus) map[string]CredentialCapabilityStatus {
	result := make(map[string]CredentialCapabilityStatus, len(source))
	for name, capability := range source {
		result[name] = capability
	}
	return result
}

func (d *Daemon) serveLocalCredentialRoute(ctx context.Context, client net.Conn, hello credentialStreamHello, claim *CredentialClaim) {
	key := credentialSSHSourceKey("", hello.Workspace, hello.Generation)
	switch hello.Capability {
	case credentialCapabilitySSH:
		d.credentialMu.Lock()
		socket := d.credentialSSHSources[key]
		d.credentialMu.Unlock()
		if socket == "" && d.sshAgent.EffectiveSocket != "" {
			socket = d.sshAgent.EffectiveSocket
		}
		upstream, err := dialAgentSocket(socket)
		if err == nil {
			defer upstream.Close()
			proxyCredentialConnections(ctx, client, upstream)
		}
	case credentialCapabilityOpenPGP:
		d.credentialMu.Lock()
		manifest := cloneCredentialOpenPGPManifest(d.credentialOpenPGP[key])
		d.credentialMu.Unlock()
		if manifest == nil {
			bundle, ok := d.config.credentialBundle(claim.Bundle)
			if !ok {
				return
			}
			resolved, err := buildOpenPGPManifest(ctx, d.config, bundle.OpenPGP.SigningKeys, d.providerRunner())
			if err != nil {
				return
			}
			manifest = resolved
			d.credentialMu.Lock()
			d.credentialOpenPGP[key] = cloneCredentialOpenPGPManifest(resolved)
			d.credentialMu.Unlock()
		}
		_ = d.filterOpenPGPStream(ctx, "", hello, manifest, client)
	case credentialCapabilityPIVB:
		if claim.PIVB != nil {
			_ = d.proxyPIVBMint(ctx, client, hello.Workspace, hello.Bundle, hello.Generation, claim.OwnerAttachmentID, claim.OwnerNodeID, claim.PIVB, claim.WindowSeconds, claim.UpdatedAt)
		}
	}
}

func (d *Daemon) serveRemoteCredentialRoute(ctx context.Context, client net.Conn, hello credentialStreamHello, claim *CredentialClaim, originNodeID string) {
	d.credentialMu.Lock()
	transport := d.credentialTransports[claim.OwnerNodeID]
	d.credentialMu.Unlock()
	if transport.State != "ready" || transport.Endpoint == "" || time.Since(transport.UpdatedAt) > 3*time.Second {
		d.writeCredentialRouteUnavailable(client, hello.Capability, "credential provider transport is unavailable")
		return
	}
	stream, err := net.DialTimeout("unix", transport.Endpoint, 500*time.Millisecond)
	if err != nil {
		d.writeCredentialRouteUnavailable(client, hello.Capability, "credential provider transport is unavailable")
		return
	}
	defer stream.Close()
	if err := writeCredentialStreamHello(stream, hello); err != nil {
		return
	}
	if hello.Capability == credentialCapabilityPIVB && claim.PIVB != nil {
		// PIVB responses are inspected and rebound below, but the request still
		// has to travel to the provider concurrently (HTTP clients do not close
		// the connection after writing a request).
		go func() { _, _ = io.Copy(stream, client) }()
		// The origin stamps the same window pair the provider will stamp from its
		// mirror of this claim, so the forward context the pane reads back reports
		// the grant the mint was actually made under.
		grantWindow, grantDeadline := pivbGrantWindow(claim.WindowSeconds, claim.UpdatedAt, time.Now())
		ctx := pivbForwardContext{
			OriginNodeID: originNodeID, WorkspaceID: hello.Workspace, Bundle: hello.Bundle,
			ClaimGeneration: hello.Generation, ProviderNodeID: claim.OwnerNodeID, ProviderAttachID: claim.OwnerAttachmentID,
			WindowSeconds: grantWindow, WindowDeadline: grantDeadline,
		}
		if bound, succeeded := proxyRemotePIVBResponse(stream, client, claim.PIVB.Card, ctx); succeeded {
			d.logger.Printf("PIVB route mint succeeded attachment_mode=route-required protocol=%d route=remote workspace=%s bundle=%s generation=%d provider_node=%s provider_attachment=%s operation=%s",
				managedPIVBAttachmentProtocol(d.config), hello.Workspace, hello.Bundle, hello.Generation, claim.OwnerNodeID, claim.OwnerAttachmentID, bound.OperationID)
		}
		return
	}
	proxyCredentialConnections(ctx, client, stream)
}

func proxyRemotePIVBResponse(stream net.Conn, client net.Conn, expected CredentialPIVBCard, trusted pivbForwardContext) (pivbForwardContext, bool) {
	bound, succeeded, err := proxyBoundPIVBResponse(stream, client, expected, trusted)
	if err != nil {
		status, code, message := http.StatusServiceUnavailable, "PIVB_UNAVAILABLE", "remote PIVB route is unavailable: "
		if errors.Is(err, errPIVBRemoteResponseBinding) {
			status, code, message = http.StatusForbidden, "PIVB_CONFIG", "remote PIVB response rejected by origin: "
		}
		_ = writePIVBProxyError(client, status, code, message+err.Error())
		return trusted, false
	}
	return bound, succeeded
}

func (d *Daemon) writeCredentialRouteUnavailable(conn net.Conn, capability, detail string) {
	if capability == credentialCapabilityPIVB {
		_ = writePIVBProxyError(conn, http.StatusServiceUnavailable, "PIVB_UNAVAILABLE", detail)
	}
}

func (d *Daemon) validateCredentialRoutes(workspaceID string, capabilities map[string]CredentialCapabilityStatus) error {
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()
	for capability, status := range capabilities {
		if !status.Available {
			continue
		}
		listener := d.credentialRoutes[credentialRouteKey(workspaceID, capability)]
		if listener == nil || !listener.socketPublished() {
			return fmt.Errorf("publish stable %s credential route for workspace %s", capability, workspaceID)
		}
	}
	return nil
}

func (d *Daemon) revokeCredentialRoutes(workspace string, generation uint64) {
	d.credentialMu.Lock()
	listeners := make([]*credentialRouteListener, 0, len(d.credentialRoutes))
	for _, listener := range d.credentialRoutes {
		if listener.workspace == workspace {
			listeners = append(listeners, listener)
		}
	}
	d.credentialMu.Unlock()
	for _, listener := range listeners {
		listener.mu.Lock()
		for conn, activeGeneration := range listener.active {
			if generation == 0 || activeGeneration == generation {
				_ = conn.Close()
			}
		}
		listener.mu.Unlock()
	}
}

func (l *credentialRouteListener) socketPublished() bool {
	current, err := os.Lstat(l.path)
	return err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(l.boundInfo, current)
}

func (l *credentialRouteListener) close() {
	l.once.Do(func() {
		_ = l.listener.Close()
		l.mu.Lock()
		for conn := range l.active {
			_ = conn.Close()
		}
		l.mu.Unlock()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.boundInfo, current) {
			_ = os.Remove(l.path)
		}
	})
}

func (d *Daemon) closeAllCredentialRoutes() {
	d.credentialMu.Lock()
	listeners := make([]*credentialRouteListener, 0, len(d.credentialRoutes))
	for _, listener := range d.credentialRoutes {
		listeners = append(listeners, listener)
	}
	d.credentialRoutes = map[string]*credentialRouteListener{}
	d.credentialMu.Unlock()
	for _, listener := range listeners {
		listener.close()
	}
}

// proxyRawCredentialTransport is used by the private per-control-session
// broker. The first bytes are the credentialStreamHello written by zkad.
func proxyRawCredentialTransport(ctx context.Context, left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	select {
	case <-ctx.Done():
	case <-done:
	}
}
