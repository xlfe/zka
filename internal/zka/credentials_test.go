package zka

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCredentialSSHSourceIsScopedToClaimGeneration(t *testing.T) {
	d := &Daemon{
		sshAgent:             sshAgentInfo{EffectiveSocket: "/daemon-agent"},
		credentialSSHSources: map[string]string{},
	}
	request := workspaceCredentialRequest{Workspace: "workspace", Bundle: "work"}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	status := workspaceCredentialStatus{
		Generation: 3,
		Capabilities: map[string]credentialCapabilityView{
			credentialCapabilitySSH: {State: "ready", Available: true},
		},
	}
	result, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	d.reconcileCredentialProviderSources(remoteDaemonRequest{
		Host: "devbox", Op: "credentials_claim", Payload: payload, CallerSSHAuthSock: "/caller-agent",
	}, result)
	if got := d.credentialSSHSources[credentialSSHSourceKey("devbox", "workspace", 3)]; got != "/caller-agent" {
		t.Fatalf("claim source = %q", got)
	}
	d.sshAgent = sshAgentInfo{IdentityAgent: "/configured", EffectiveSocket: "/configured-agent"}
	status.Generation = 4
	result, _ = json.Marshal(status)
	d.reconcileCredentialProviderSources(remoteDaemonRequest{
		Host: "devbox", Op: "credentials_claim", Payload: payload, CallerSSHAuthSock: "/different-caller",
	}, result)
	if _, stale := d.credentialSSHSources[credentialSSHSourceKey("devbox", "workspace", 3)]; stale {
		t.Fatal("prior claim generation retained its SSH source")
	}
	if got := d.credentialSSHSources[credentialSSHSourceKey("devbox", "workspace", 4)]; got != "/configured-agent" {
		t.Fatalf("configured claim source = %q", got)
	}
	d.reconcileCredentialProviderSources(remoteDaemonRequest{Host: "devbox", Op: "credentials_release", Payload: payload}, []byte(`{}`))
	if len(d.credentialSSHSources) != 0 {
		t.Fatalf("release retained SSH sources: %#v", d.credentialSSHSources)
	}
}

func TestMissingCredentialSSHSourceDegradesAfterProviderRestart(t *testing.T) {
	d := &Daemon{
		state:                StateData{Node: Host{ID: "provider"}},
		credentialSSHSources: map[string]string{},
	}
	workspace := &Workspace{
		ID: "workspace", RemoteHost: "devbox",
		CredentialClaim: &CredentialClaim{
			OwnerNodeID: "provider", Generation: 3,
			Capabilities: map[string]CredentialCapabilityStatus{
				credentialCapabilitySSH: {State: "ready", Available: true},
			},
		},
	}
	status := workspaceCredentialStatus{
		State: "ready", Capabilities: map[string]credentialCapabilityView{
			credentialCapabilitySSH: {State: "ready", Available: true},
		},
	}
	d.degradeMissingCredentialSSHSource(&status, workspace)
	if status.State != "degraded" || status.Capabilities[credentialCapabilitySSH].Available ||
		!strings.Contains(status.Capabilities[credentialCapabilitySSH].Detail, "re-claim") {
		t.Fatalf("missing source status = %#v", status)
	}

	d.sshAgent = sshAgentInfo{IdentityAgent: "/configured", EffectiveSocket: "/configured"}
	status.State = "ready"
	status.Capabilities[credentialCapabilitySSH] = credentialCapabilityView{State: "ready", Available: true}
	d.degradeMissingCredentialSSHSource(&status, workspace)
	if status.State != "ready" || !status.Capabilities[credentialCapabilitySSH].Available {
		t.Fatalf("configured identity agent degraded: %#v", status)
	}
}

func readyCredentialAttachment(t *testing.T, d *Daemon, workspace *Workspace, id, node string) *Attachment {
	t.Helper()
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: id, Node: Host{ID: node, Name: node}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:" + node + ":" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Status: AttachmentReady,
		Views: map[string]RuntimeView{pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true}},
	}); err != nil {
		t.Fatal(err)
	}
	return attachment
}

func sshCredentialBundle() CredentialBundleConfig {
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	return bundle
}

func TestCredentialStatusDegradesAndRecoversWithTransport(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Bundle: "work",
		Manifest: credentialBundleManifest{Bundle: "work", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "disconnected"}); err != nil {
		t.Fatal(err)
	}
	status := d.allCredentialStatuses()
	if status.Transport.State != "degraded" || len(status.Workspaces) != 1 || status.Workspaces[0].State != "degraded" || status.Workspaces[0].Capabilities[credentialCapabilitySSH].Available {
		t.Fatalf("disconnected status = %#v", status)
	}
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	status = d.allCredentialStatuses()
	if status.Transport.State != "ready" || status.Workspaces[0].State != "ready" || !status.Workspaces[0].Capabilities[credentialCapabilitySSH].Available {
		t.Fatalf("recovered status = %#v", status)
	}
}

func TestFailedCredentialPreparationRetainsPriorClaim(t *testing.T) {
	runner := quietRunner()
	runner.handler = func(context.Context, string, ...string) (string, string, error) {
		return "", "", errors.New("gpg unavailable")
	}
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	var openpgp CredentialBundleConfig
	openpgp.OpenPGP.Enable = true
	openpgp.OpenPGP.SigningKeys = []string{testFingerprint}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"ssh": sshCredentialBundle(), "openpgp": openpgp}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	_ = d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "ready"})
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Bundle: "ssh", Manifest: credentialBundleManifest{Bundle: "ssh", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Bundle: "openpgp",
		Manifest: credentialBundleManifest{Bundle: "openpgp", OpenPGP: &credentialOpenPGPManifest{Fingerprints: []string{testFingerprint}, PublicKeys: "invalid"}},
	})
	if err == nil {
		t.Fatal("OpenPGP preparation unexpectedly succeeded")
	}
	status, err := d.workspaceCredentialStatus(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Bundle != "ssh" || status.Generation != 1 || !status.Capabilities[credentialCapabilitySSH].Available {
		t.Fatalf("prior claim was not retained: %#v", status)
	}
}

func TestConcurrentCredentialClaimsAreSerialized(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"one": sshCredentialBundle(), "two": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	_ = d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "ready"})
	var wg sync.WaitGroup
	errorsByBundle := make(chan error, 2)
	for _, bundle := range []string{"one", "two"} {
		wg.Add(1)
		go func(bundle string) {
			defer wg.Done()
			_, claimErr := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
				Workspace: workspace.ID, Attachment: attachment.ID, Bundle: bundle,
				Manifest: credentialBundleManifest{Bundle: bundle, SSH: true},
			})
			errorsByBundle <- claimErr
		}(bundle)
	}
	wg.Wait()
	close(errorsByBundle)
	for claimErr := range errorsByBundle {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	status, err := d.workspaceCredentialStatus(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Generation != 2 || status.Bundle != "one" && status.Bundle != "two" {
		t.Fatalf("serialized claim = %#v", status)
	}
}
