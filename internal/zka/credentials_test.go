package zka

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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

	d.sshAgent = sshAgentInfo{}
	workspace.RemoteHost = ""
	workspace.CredentialClaim.ProviderSource = "local"
	status.State = "ready"
	status.Capabilities[credentialCapabilitySSH] = credentialCapabilityView{State: "ready", Available: true}
	d.degradeMissingCredentialSSHSource(&status, workspace)
	if status.State != "degraded" || status.Capabilities[credentialCapabilitySSH].Available ||
		!strings.Contains(status.Capabilities[credentialCapabilitySSH].Detail, "reactivate local") {
		t.Fatalf("missing local source status = %#v", status)
	}
}

func TestCredentialTransportStatusRequiresEveryInboundProvider(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	one := Host{ID: "first-provider"}
	two := Host{ID: "second-provider"}
	readyCredentialTransport(t, d, one)
	workspaces := []*Workspace{
		{ID: "one", CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: one.ID}},
		{ID: "two", CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: two.ID}},
	}
	if status := d.credentialTransportStatus(workspaces); status.State != "degraded" {
		t.Fatalf("one missing inbound provider status = %#v", status)
	}
	readyCredentialTransport(t, d, two)
	if status := d.credentialTransportStatus(workspaces); status.State != "ready" {
		t.Fatalf("all inbound providers ready status = %#v", status)
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

func TestCredentialStatusReportsPaneEnvironmentMigrationAndClaimGaps(t *testing.T) {
	workspace := &Workspace{
		ID: "workspace", Name: "reviewer",
		Panes: map[string]*Pane{
			"local":  {ID: "local", BackendCreated: true},
			"legacy": {ID: "legacy", BackendCreated: true, CredentialEnvironmentVersion: legacyCredentialEnvironmentVersion},
			"remote": {ID: "remote", BackendCreated: true, CredentialEnvironmentVersion: credentialEnvironmentVersion},
			"dead":   {ID: "dead", BackendCreated: true, BackendDead: true, CredentialEnvironmentVersion: legacyCredentialEnvironmentVersion},
		},
	}

	unclaimed := credentialStatusFromWorkspace(workspace)
	if got, want := strings.Join(unclaimed.RecreatePaneIDs, ","), "local"; got != want {
		t.Fatalf("unclaimed recreation panes = %q, want %q", got, want)
	}
	if !strings.Contains(unclaimed.RecreationDetail, "version 0") || !strings.Contains(unclaimed.RecreationDetail, "automatic") || len(unclaimed.Capabilities) != 0 {
		t.Fatalf("unclaimed recreation status = %#v", unclaimed)
	}

	workspace.CredentialClaim = &CredentialClaim{
		Bundle: "work", OwnerNodeID: "provider", State: "ready",
		Capabilities: map[string]CredentialCapabilityStatus{
			credentialCapabilitySSH:     {State: "ready", Available: true},
			credentialCapabilityOpenPGP: {State: "ready", Available: true, Detail: "warning: signing key is not card-backed"},
		},
	}
	claimed := credentialStatusFromWorkspace(workspace)
	if got, want := strings.Join(claimed.RecreatePaneIDs, ","), "local"; got != want {
		t.Fatalf("claimed recreation panes = %q, want %q", got, want)
	}
	if !strings.Contains(claimed.RecreationDetail, "version 0") || !strings.Contains(claimed.RecreationDetail, "transfer is blocked") {
		t.Fatalf("claimed recreation detail = %q", claimed.RecreationDetail)
	}
	if detail := claimed.Capabilities[credentialCapabilitySSH].Detail; !strings.Contains(detail, "SSH_AUTH_SOCK") {
		t.Fatalf("SSH recreation detail = %q", detail)
	}
	if detail := claimed.Capabilities[credentialCapabilityOpenPGP].Detail; !strings.Contains(detail, "not card-backed") || !strings.Contains(detail, "GNUPGHOME") {
		t.Fatalf("OpenPGP recreation detail lost warning or guidance: %q", detail)
	}
	for _, test := range []struct {
		name       string
		capability string
		endpoint   string
	}{
		{name: "ssh-only", capability: credentialCapabilitySSH, endpoint: "SSH_AUTH_SOCK"},
		{name: "openpgp-only", capability: credentialCapabilityOpenPGP, endpoint: "GNUPGHOME"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace.CredentialClaim.Capabilities = map[string]CredentialCapabilityStatus{
				test.capability: {State: "ready", Available: true},
			}
			status := credentialStatusFromWorkspace(workspace)
			if got := strings.Join(status.RecreatePaneIDs, ","); got != "local" {
				t.Fatalf("recreation panes = %q", got)
			}
			if detail := status.Capabilities[test.capability].Detail; !strings.Contains(detail, test.endpoint) {
				t.Fatalf("capability detail = %q", detail)
			}
		})
	}

	workspace.Panes["local"].CredentialEnvironmentVersion = credentialEnvironmentVersion
	workspace.Panes["legacy"].CredentialEnvironmentVersion = credentialEnvironmentVersion
	current := credentialStatusFromWorkspace(workspace)
	if len(current.RecreatePaneIDs) != 0 || current.RecreationDetail != "" {
		t.Fatalf("current remote panes require recreation: %#v", current)
	}
}

func TestCredentialStatusDegradesAndRecoversWithTransport(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	endpoint := readyCredentialTransport(t, d, attachment.Node)
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "work",
		Manifest: credentialBundleManifest{Bundle: "work", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "disconnected", Endpoint: endpoint}); err != nil {
		t.Fatal(err)
	}
	status := d.allCredentialStatuses()
	if status.Transport.State != "degraded" || len(status.Workspaces) != 1 || status.Workspaces[0].State != "degraded" || status.Workspaces[0].Capabilities[credentialCapabilitySSH].Available {
		t.Fatalf("disconnected status = %#v", status)
	}
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: attachment.Node, State: "ready", Endpoint: endpoint}); err != nil {
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
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	var openpgp CredentialBundleConfig
	openpgp.OpenPGP.Enable = true
	openpgp.OpenPGP.SigningKeys = []string{testFingerprint}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"ssh": sshCredentialBundle(), "openpgp": openpgp}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	readyCredentialTransport(t, d, attachment.Node)
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "ssh", Manifest: credentialBundleManifest{Bundle: "ssh", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "openpgp",
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

func TestActivateLocalIfUnclaimedDoesNotMutateExistingProviderSources(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{
		"work": sshCredentialBundle(), "personal": sshCredentialBundle(),
	}
	workspace := createTestWorkspace(t, d, 1)
	workPath := filepath.Join(d.paths.RuntimeDir, "work-agent.sock")
	workAgent, err := listenUnix(workPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workAgent.Close() })
	go serveCredentialTestByte(workAgent, 'W')
	personalPath := filepath.Join(d.paths.RuntimeDir, "personal-agent.sock")
	personalAgent, err := listenUnix(personalPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = personalAgent.Close() })
	go serveCredentialTestByte(personalAgent, 'P')

	bound, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, workPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := credentialTestRoundTrip(t, agentRelaySocketPath(d.paths.AgentDir, workspace.ID), 'a'); got != 'W' {
		t.Fatalf("initial provider reply = %q", got)
	}
	untouched, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "personal", true, personalPath)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Bundle != "work" || untouched.Generation != bound.Generation {
		t.Fatalf("if-unclaimed status = %#v, want original work binding", untouched)
	}
	if got := credentialTestRoundTrip(t, agentRelaySocketPath(d.paths.AgentDir, workspace.ID), 'b'); got != 'W' {
		t.Fatalf("if-unclaimed changed provider reply to %q", got)
	}
}

func TestConcurrentCredentialClaimsAreSerialized(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"one": sshCredentialBundle(), "two": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	readyCredentialTransport(t, d, attachment.Node)
	var wg sync.WaitGroup
	errorsByBundle := make(chan error, 2)
	for _, bundle := range []string{"one", "two"} {
		wg.Add(1)
		go func(bundle string) {
			defer wg.Done()
			_, claimErr := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
				Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: bundle,
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

func TestCredentialGenerationRemainsMonotonicAcrossRelease(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	attachment := readyCredentialAttachment(t, d, workspace, "provider-attachment", "provider")
	readyCredentialTransport(t, d, attachment.Node)
	request := workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "work",
		Manifest: credentialBundleManifest{Bundle: "work", SSH: true},
	}
	first, err := d.claimWorkspaceCredentials(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.releaseWorkspaceCredentials(workspace.ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.claimWorkspaceCredentials(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("generation after release = %d, want > %d", second.Generation, first.Generation)
	}
	d.mu.Lock()
	storedGeneration := d.state.Workspaces[workspace.ID].CredentialGeneration
	d.mu.Unlock()
	if storedGeneration != second.Generation {
		t.Fatalf("stored credential generation = %d, want %d", storedGeneration, second.Generation)
	}
}
