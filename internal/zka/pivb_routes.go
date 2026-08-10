package zka

import (
	"context"
	"fmt"
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

func (d *Daemon) activateLocalCredentialBundle(ctx context.Context, workspaceRef, bundleName string, ifUnclaimed bool, callerSSHAuthSock string) (workspaceCredentialStatus, error) {
	bundle, ok := d.config.credentialBundle(bundleName)
	if !ok {
		return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q is not configured on this node", bundleName)
	}
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	provider := d.state.Node
	workspaceID := workspace.ID
	d.mu.Unlock()

	socket := d.credentialSSHSocketForCaller(callerSSHAuthSock)
	manifest, err := buildCredentialBundleManifestForSocket(ctx, d.config, bundleName, d.runner, socket)
	if err != nil {
		return workspaceCredentialStatus{}, err
	}
	status, err := d.claimWorkspaceCredentials(ctx, workspaceCredentialRequest{
		Workspace: workspaceID, Bundle: bundleName, IfUnclaimed: ifUnclaimed,
		Provider: provider, ProviderSource: "local", Manifest: manifest,
	})
	if err != nil {
		return status, err
	}
	if !status.providerSelected || status.OwnerNode != provider.ID {
		return status, nil
	}
	d.clearCredentialProviderSources("", workspaceID)
	key := credentialSSHSourceKey("", workspaceID, status.Generation)
	if bundle.SSHAgent.Enable {
		d.setCredentialSSHSource("", workspaceID, status.Generation, socket)
	}
	if bundle.OpenPGP.Enable && manifest.OpenPGP != nil {
		d.credentialMu.Lock()
		d.credentialOpenPGP[key] = cloneCredentialOpenPGPManifest(manifest.OpenPGP)
		d.credentialMu.Unlock()
	}
	return d.workspaceCredentialStatus(workspaceID)
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
	if provider := workspace.CredentialClaim; provider != nil {
		result.State, result.Source, result.Bundle, result.Generation = provider.State, provider.ProviderSource, provider.Bundle, provider.Generation
		if result.State == "ready" && !credentialSocketPublished(result.Socket) {
			result.State = "degraded"
			result.Detail = appendCredentialDetail(result.Detail, "workspace PIVB route listener is not published")
		}
	}
	return result, nil
}
