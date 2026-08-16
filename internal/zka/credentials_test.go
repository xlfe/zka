package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialSSHSourceIsScopedToClaimGeneration(t *testing.T) {
	root := testRoot(t)
	d, journal, err := newTestDaemonWithLog(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.sshAgent = sshAgentInfo{EffectiveSocket: "/daemon-agent"}
	// Every provider-source change also asks pivbd to retire the reuse state
	// behind it. Pointing that at a dead socket proves the bookkeeping below
	// survives an unreachable pivbd, and that the failure is journalled.
	d.config.Credentials.PIVB.ForwardSocket = deadUnixSocket(t, filepath.Join(root, "dead-forward.sock"))
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
	waitFor(t, func() bool { return strings.Count(journal.String(), "PIVB reuse invalidation failed") == 3 })
}

// deadUnixSocket leaves a bound socket file behind with nothing accepting on
// it, which is what a crashed daemon leaves and what a missing one does not.
func deadUnixSocket(t *testing.T, path string) string {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
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
	if got, want := strings.Join(unclaimed.RecreatePaneIDs, ","), "legacy,local"; got != want {
		t.Fatalf("unclaimed recreation panes = %q, want %q", got, want)
	}
	if !strings.Contains(unclaimed.RecreationDetail, "explicit") || len(unclaimed.Capabilities) != 0 {
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
	if got, want := strings.Join(claimed.RecreatePaneIDs, ","), "legacy,local"; got != want {
		t.Fatalf("claimed recreation panes = %q, want %q", got, want)
	}
	if !strings.Contains(claimed.RecreationDetail, "explicit") || !strings.Contains(claimed.RecreationDetail, "transfer is blocked") {
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
			if got := strings.Join(status.RecreatePaneIDs, ","); got != "legacy,local" {
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
		OwnerAttachmentID: attachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", SSH: true},
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
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "ssh", OwnerAttachmentID: attachment.ID, Manifest: credentialBundleManifest{Bundle: "ssh", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "openpgp",
		OwnerAttachmentID: attachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "openpgp", OpenPGP: &credentialOpenPGPManifest{Fingerprints: []string{testFingerprint}, PublicKeys: "invalid"}},
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

func TestPrepareOpenPGPTargetDefersImportFailureToFingerprintValidation(t *testing.T) {
	const (
		importStdout = "gpg: imported 1\n"
		importStderr = "gpg: no gpg-agent running in this session\ngpg: can't connect to the gpg-agent: End of file\n"
	)
	tests := []struct {
		name        string
		listing     string
		wantSuccess bool
	}{
		{
			name:        "valid fingerprint",
			listing:     "fpr:::::::::" + testFingerprint + ":\n",
			wantSuccess: true,
		},
		{
			name: "missing fingerprint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := testRoot(t)
			paths := testPaths(root)
			cfg := defaultConfig()
			agentSocket := filepath.Join(root, "agent.sock")
			runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
				joined := strings.Join(args, " ")
				switch {
				case name == cfg.Credentials.GnuPG.GPGConfCommand && strings.HasSuffix(joined, "--list-dirs socketdir"):
					return root, "", nil
				case name == cfg.Credentials.GnuPG.Command && strings.Contains(joined, " --import "):
					return importStdout, importStderr, errors.New("exit status 2")
				case name == cfg.Credentials.GnuPG.Command && strings.Contains(joined, " --list-keys"):
					return tt.listing, "", nil
				case name == cfg.Credentials.GnuPG.GPGConfCommand && strings.HasSuffix(joined, "--list-dirs agent-socket"):
					return agentSocket, "", nil
				default:
					return "", "", fmt.Errorf("unexpected command: %s %s", name, joined)
				}
			}}
			journal := &syncBuffer{}
			logger := log.New(journal, "", 0)
			workspaceID := "workspace-" + strings.ReplaceAll(tt.name, " ", "-")
			socket, err := prepareOpenPGPTarget(context.Background(), paths, cfg, workspaceID, &credentialOpenPGPManifest{
				Fingerprints: []string{testFingerprint},
				PublicKeys:   "public key data",
			}, runner, logger)

			if tt.wantSuccess {
				if err != nil || socket != agentSocket {
					t.Fatalf("prepare OpenPGP target = %q, %v; want %q, nil", socket, err, agentSocket)
				}
				entry := journal.String()
				for _, want := range []string{
					"OpenPGP import advisory (expected)",
					"workspace " + workspaceID,
					"fingerprint validation passed",
					"gpg: no gpg-agent running in this session gpg: can't connect to the gpg-agent: End of file",
				} {
					if !strings.Contains(entry, want) {
						t.Fatalf("advisory log %q does not contain %q", entry, want)
					}
				}
				if strings.Count(entry, "\n") != 1 {
					t.Fatalf("advisory log was not collapsed to one line: %q", entry)
				}
				return
			}

			if err == nil {
				t.Fatal("OpenPGP preparation succeeded without the configured fingerprint")
			}
			for _, want := range []string{testFingerprint, "gpg: imported 1", "no gpg-agent running", "End of file", "exit status 2"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("preparation error %q does not contain %q", err, want)
				}
			}
			if entry := journal.String(); entry != "" {
				t.Fatalf("failed fingerprint validation was also logged: %q", entry)
			}
		})
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
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
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

	bound, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, workPath, localOwner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := credentialTestRoundTrip(t, agentRelaySocketPath(d.paths.AgentDir, workspace.ID), 'a'); got != 'W' {
		t.Fatalf("initial provider reply = %q", got)
	}
	untouched, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "personal", true, personalPath, localOwner.ID, 0)
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

func TestActivateLocalIfUnclaimedDoesNotRequireOwnerOrProbeProvider(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{
		"work": sshCredentialBundle(), "personal": sshCredentialBundle(),
	}
	workspace := createTestWorkspace(t, d, 1)
	remoteOwner := readyCredentialAttachment(t, d, workspace, "remote-owner", "provider")
	readyCredentialTransport(t, d, remoteOwner.Node)
	claimed, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: remoteOwner.Node, ProviderSource: "remote",
		OwnerAttachmentID: remoteOwner.ID, Manifest: credentialBundleManifest{Bundle: "work", SSH: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	sentinelKey := credentialSSHSourceKey("", workspace.ID, claimed.Generation)
	d.credentialMu.Lock()
	d.credentialSSHSources[sentinelKey] = "/sentinel/ssh-agent.sock"
	d.credentialOpenPGP[sentinelKey] = &credentialOpenPGPManifest{Fingerprints: []string{"sentinel"}}
	d.credentialMu.Unlock()
	callsBefore := len(runner.Calls())

	untouched, err := d.activateLocalCredentialBundle(
		context.Background(), workspace.ID, "personal", true,
		filepath.Join(d.paths.RuntimeDir, "missing-agent.sock"), "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Bundle != "work" || untouched.OwnerNode != remoteOwner.Node.ID || untouched.Generation != claimed.Generation {
		t.Fatalf("if-unclaimed status = %#v, want original remote binding", untouched)
	}
	if callsAfter := len(runner.Calls()); callsAfter != callsBefore {
		t.Fatalf("if-unclaimed probed provider commands: before=%d after=%d calls=%#v", callsBefore, callsAfter, runner.Calls())
	}
	d.credentialMu.Lock()
	sshSource := d.credentialSSHSources[sentinelKey]
	openPGP := d.credentialOpenPGP[sentinelKey]
	d.credentialMu.Unlock()
	if sshSource == "" || openPGP == nil || len(openPGP.Fingerprints) != 1 || openPGP.Fingerprints[0] != "sentinel" {
		t.Fatalf("if-unclaimed cleared provider sources: ssh=%q openpgp=%#v", sshSource, openPGP)
	}
}

func TestActivateLocalCredentialBundleRequiresExplicitOwnerAttachment(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)

	_, err = d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "owner attachment is required") {
		t.Fatalf("activation without explicit owner = %v", err)
	}
}

// windowedCredentialDaemon returns an origin whose only bundle is PIVB-backed
// by a provider granting maxGrantWindow, plus the local attachment that owns
// its claims.
func windowedCredentialDaemon(t *testing.T, maxGrantWindow int64) (*Daemon, *fakePIVBProvider, *Workspace, *Attachment) {
	t.Helper()
	provider := newFakePIVBProvider(t, maxGrantWindow)
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": pivbCredentialBundle(), "ssh": sshCredentialBundle()}
	d.config.Credentials.PIVB.ForwardSocket = provider.socket
	workspace := createTestWorkspace(t, d, 1)
	owner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	return d, provider, workspace, owner
}

func credentialClaimSnapshot(t *testing.T, d *Daemon, workspaceID string) CredentialClaim {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	claim := d.state.Workspaces[workspaceID].CredentialClaim
	if claim == nil {
		t.Fatal("workspace has no credential claim")
	}
	return *claim
}

func TestCredentialClaimRecordsGrantWindowAndProvisionalDeadline(t *testing.T) {
	d, _, workspace, owner := windowedCredentialDaemon(t, 3600)
	status, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if status.WindowSeconds != 1800 || status.WindowGranted != 1800 {
		t.Fatalf("windowed claim status = %#v", status)
	}
	claim := credentialClaimSnapshot(t, d, workspace.ID)
	if claim.WindowSeconds != 1800 {
		t.Fatalf("persisted claim window = %d, want 1800", claim.WindowSeconds)
	}
	if want := claim.UpdatedAt.Add(1800 * time.Second).Unix(); status.WindowDeadline != want {
		t.Fatalf("window deadline = %d, want %d (claim written at %s)", status.WindowDeadline, want, claim.UpdatedAt)
	}

	// The window is recorded as requested and clamped only in effect, so an
	// operator can still see what they asked for after the provider trims it.
	clamped, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 7200)
	if err != nil {
		t.Fatal(err)
	}
	if clamped.WindowSeconds != 7200 || clamped.WindowGranted != 3600 {
		t.Fatalf("clamped claim status = %#v, want 7200 requested and 3600 granted", clamped)
	}
	clampedClaim := credentialClaimSnapshot(t, d, workspace.ID)
	if want := clampedClaim.UpdatedAt.Add(3600 * time.Second).Unix(); clamped.WindowDeadline != want {
		t.Fatalf("clamped deadline = %d, want %d", clamped.WindowDeadline, want)
	}
}

func TestWindowedReclaimRegrantsWhileWindowlessReclaimStaysANoOp(t *testing.T) {
	d, _, workspace, owner := windowedCredentialDaemon(t, 3600)
	windowed, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 600)
	if err != nil {
		t.Fatal(err)
	}
	first := credentialClaimSnapshot(t, d, workspace.ID)

	// Renewing a window is a re-grant even when nothing else about the claim
	// moved: the deadline is anchored to the claim, and only a new generation
	// makes the provider re-read it.
	time.Sleep(10 * time.Millisecond)
	renewed, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Generation <= windowed.Generation {
		t.Fatalf("windowed renewal generation = %d, want > %d", renewed.Generation, windowed.Generation)
	}
	second := credentialClaimSnapshot(t, d, workspace.ID)
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("windowed renewal left the anchor at %s", second.UpdatedAt)
	}
	if want := second.UpdatedAt.Add(600 * time.Second).Unix(); renewed.WindowDeadline != want {
		t.Fatalf("windowed renewal deadline = %d, want %d re-anchored to %s", renewed.WindowDeadline, want, second.UpdatedAt)
	}

	// Dropping the window is a real change, so it re-grants windowlessly.
	closed, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Generation <= renewed.Generation || closed.WindowSeconds != 0 || closed.WindowDeadline != 0 {
		t.Fatalf("windowless re-claim over a windowed claim = %#v", closed)
	}
	if got := credentialClaimSnapshot(t, d, workspace.ID); got.WindowSeconds != 0 {
		t.Fatalf("persisted claim kept window %d after a windowless re-claim", got.WindowSeconds)
	}

	// A windowless claim over a windowless claim keeps today's no-op.
	repeated, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Generation != closed.Generation {
		t.Fatalf("windowless re-claim changed generation from %d to %d", closed.Generation, repeated.Generation)
	}
}

func TestGrantWindowIsRefusedWithoutAProviderThatGrantsOne(t *testing.T) {
	d, _, workspace, owner := windowedCredentialDaemon(t, 0)
	_, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 600)
	if err == nil || !strings.Contains(err.Error(), "max_grant_window_s is 0") ||
		!strings.Contains(err.Error(), fakePIVBProviderResource) {
		t.Fatalf("window against a provider that grants none = %v", err)
	}
	if claim := d.state.Workspaces[workspace.ID].CredentialClaim; claim != nil {
		t.Fatalf("refused window still published a claim: %#v", claim)
	}

	sshPath := filepath.Join(d.paths.RuntimeDir, "ssh-agent.sock")
	sshAgent, err := listenUnix(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshAgent.Close() })
	_, err = d.activateLocalCredentialBundle(context.Background(), workspace.ID, "ssh", false, sshPath, owner.ID, 600)
	if err == nil || !strings.Contains(err.Error(), "does not enable PIVB") {
		t.Fatalf("window on a bundle without PIVB = %v", err)
	}

	_, err = d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: d.state.Node, ProviderSource: "local", Bundle: "ssh",
		OwnerAttachmentID: owner.ID, WindowSeconds: -1,
		Manifest: credentialBundleManifest{Bundle: "ssh", SSH: true},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative window = %v", err)
	}
}

func TestReleasingALocalPIVBClaimInvalidatesProviderReuse(t *testing.T) {
	d, provider, workspace, owner := windowedCredentialDaemon(t, 3600)
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.releaseWorkspaceCredentials(workspace.ID); err != nil {
		t.Fatal(err)
	}
	request := provider.nextInvalidation(t)
	if request.Version != pivbForwardProtocolVersion || request.WorkspaceID != workspace.ID || request.ClaimGeneration != 0 {
		t.Fatalf("release invalidation = %#v", request)
	}
}

func TestPIVBReuseInvalidationIsSilentWithoutAProvider(t *testing.T) {
	root := testRoot(t)
	d, journal, err := newTestDaemonWithLog(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.PIVB.ForwardSocket = filepath.Join(root, "absent-forward.sock")
	// No pivbd means no reuse state to retire, so this reports nothing at all:
	// an SSH-only deployment must not journal a PIVB failure per claim.
	d.invalidatePIVBReuse("workspace", 0)
	if entry := journal.String(); entry != "" {
		t.Fatalf("absent PIVB provider was journalled: %q", entry)
	}
}

func TestOwnerDetachInvalidatesLocalPIVBReuse(t *testing.T) {
	d, provider, workspace, owner := windowedCredentialDaemon(t, 3600)
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.detachAttachment(workspace.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	request := provider.nextInvalidation(t)
	if request.WorkspaceID != workspace.ID || request.ClaimGeneration != 0 {
		t.Fatalf("owner detach invalidation = %#v", request)
	}
}

func TestProviderSideReconcileInvalidatesSupersededGenerations(t *testing.T) {
	provider := newFakePIVBProvider(t, 3600)
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.PIVB.ForwardSocket = provider.socket
	payload, err := json.Marshal(workspaceCredentialRequest{Workspace: "workspace", Bundle: "work"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := json.Marshal(workspaceCredentialStatus{
		Generation:   7,
		Capabilities: map[string]credentialCapabilityView{credentialCapabilityPIVB: {State: "ready", Available: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.reconcileCredentialProviderSources(remoteDaemonRequest{Host: "devbox", Op: "credentials_claim", Payload: payload}, claimed)
	if request := provider.nextInvalidation(t); request.WorkspaceID != "workspace" || request.ClaimGeneration != 7 {
		t.Fatalf("claim invalidation = %#v, want generation 7 retained", request)
	}
	d.reconcileCredentialProviderSources(remoteDaemonRequest{Host: "devbox", Op: "credentials_release", Payload: payload}, []byte(`{}`))
	if request := provider.nextInvalidation(t); request.WorkspaceID != "workspace" || request.ClaimGeneration != 0 {
		t.Fatalf("release invalidation = %#v, want every generation purged", request)
	}
}

func TestMirroredOriginClaimChangeInvalidatesProviderReuse(t *testing.T) {
	provider := newFakePIVBProvider(t, 3600)
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.PIVB.ForwardSocket = provider.socket
	remote := &Workspace{
		ID: "0123456789abcdef0123456789abcdef", Name: "reviewer",
		Origin: Host{ID: "origin-node", Name: "devbox"},
		Panes:  map[string]*Pane{}, Attachments: map[string]*Attachment{},
		CredentialClaim: &CredentialClaim{
			ProviderSource: "remote", Bundle: "work", OwnerNodeID: d.state.Node.ID, Generation: 4,
			State: "ready", Capabilities: map[string]CredentialCapabilityStatus{}, PIVB: provider.manifest(),
		},
	}
	if _, err := d.cacheRemoteWorkspace("devbox", remote); err != nil {
		t.Fatal(err)
	}

	regranted := remote.Clone()
	regranted.CredentialClaim.Generation = 5
	if _, err := d.cacheRemoteWorkspace("devbox", regranted); err != nil {
		t.Fatal(err)
	}
	if request := provider.nextInvalidation(t); request.WorkspaceID != remote.ID || request.ClaimGeneration != 5 {
		t.Fatalf("mirrored re-grant invalidation = %#v", request)
	}

	released := regranted.Clone()
	released.CredentialClaim = nil
	if _, err := d.cacheRemoteWorkspace("devbox", released); err != nil {
		t.Fatal(err)
	}
	if request := provider.nextInvalidation(t); request.WorkspaceID != remote.ID || request.ClaimGeneration != 0 {
		t.Fatalf("mirrored release invalidation = %#v", request)
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
				OwnerAttachmentID: attachment.ID,
				Manifest:          credentialBundleManifest{Bundle: bundle, SSH: true},
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
		OwnerAttachmentID: attachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", SSH: true},
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
