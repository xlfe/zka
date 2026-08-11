package zka

import (
	"context"
	"fmt"
	"sort"
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

func (d *Daemon) activateLocalCredentialBundle(ctx context.Context, workspaceRef, bundleName string, ifUnclaimed bool, callerSSHAuthSock, ownerAttachment string) (workspaceCredentialStatus, error) {
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
	if err := requireWorkspaceMutable(workspace); err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	provider := d.state.Node
	workspaceID := workspace.ID
	d.mu.Unlock()

	if ifUnclaimed {
		claimLock := d.credentialClaimLock(workspaceID)
		claimLock.Lock()
		d.mu.Lock()
		workspace, err = d.resolveWorkspaceLocked(workspaceID)
		if err != nil {
			d.mu.Unlock()
			claimLock.Unlock()
			return workspaceCredentialStatus{}, err
		}
		if workspace.RemoteHost != "" {
			d.mu.Unlock()
			claimLock.Unlock()
			return workspaceCredentialStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
		}
		if err := requireWorkspaceMutable(workspace); err != nil {
			d.mu.Unlock()
			claimLock.Unlock()
			return workspaceCredentialStatus{}, err
		}
		if workspace.CredentialClaim != nil {
			workspaceSnapshot := workspace.Clone()
			d.mu.Unlock()
			claimLock.Unlock()
			return d.credentialStatusForWorkspace(workspaceSnapshot), nil
		}
		d.mu.Unlock()
		claimLock.Unlock()
	}
	if ownerAttachment == "" {
		return workspaceCredentialStatus{}, fmt.Errorf("owner attachment is required to activate workspace credentials")
	}

	socket := d.credentialSSHSocketForCaller(callerSSHAuthSock)
	manifest, err := buildCredentialBundleManifestForSocket(ctx, d.config, bundleName, d.providerRunner(), socket)
	if err != nil {
		return workspaceCredentialStatus{}, err
	}
	status, err := d.claimWorkspaceCredentials(ctx, workspaceCredentialRequest{
		Workspace: workspaceID, Bundle: bundleName, IfUnclaimed: ifUnclaimed,
		Provider: provider, ProviderSource: "local", OwnerAttachmentID: ownerAttachment, Manifest: manifest,
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
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return pivbEndpointResponse{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return pivbEndpointResponse{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	workspaceSnapshot := workspace.Clone()
	d.mu.Unlock()

	status := d.credentialStatusForWorkspace(workspaceSnapshot)
	result := pivbEndpointResponse{
		WorkspaceID: workspaceSnapshot.ID,
		Socket:      pivbRelaySocketPath(d.paths, workspaceSnapshot.ID),
		State:       status.State,
		Bundle:      status.Bundle,
		Generation:  status.Generation,
	}
	if claim := workspaceSnapshot.CredentialClaim; claim != nil {
		result.Source = claim.ProviderSource
	}
	if result.State == "unclaimed" {
		return result, nil
	}

	pivb, hasPIVB := status.Capabilities[credentialCapabilityPIVB]
	if !hasPIVB {
		result.State = "degraded"
		result.Detail = appendCredentialDetail(result.Detail, "credential bundle does not enable PIVB")
	} else if pivb.State != "ready" || !pivb.Available {
		result.State = "degraded"
	}
	capabilityNames := make([]string, 0, len(status.Capabilities))
	for name := range status.Capabilities {
		capabilityNames = append(capabilityNames, name)
	}
	sort.Strings(capabilityNames)
	for _, name := range capabilityNames {
		capability := status.Capabilities[name]
		if capability.State == "ready" && capability.Available {
			continue
		}
		result.State = "degraded"
		detail := capability.Detail
		if detail == "" {
			detail = "state is " + capability.State
		}
		result.Detail = appendCredentialDetail(result.Detail, fmt.Sprintf("%s: %s", name, detail))
	}
	if len(status.RecreatePaneIDs) != 0 {
		result.State = "degraded"
		result.Detail = appendCredentialDetail(result.Detail,
			fmt.Sprintf("pane credential environment does not match routing mode %s; run `zka workspace reconcile --recreate-backends %s`", d.config.Credentials.PIVB.RoutingMode, workspaceSnapshot.ID))
	}
	if result.State != "ready" && result.Detail == "" {
		result.Detail = "credential bundle is not ready"
	}
	return result, nil
}
