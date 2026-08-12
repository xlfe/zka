package zka

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	remoteAttachRegressionHelperEnv       = "GO_WANT_ZKA_REMOTE_ATTACH_HELPER"
	remoteAttachRegressionOriginRootEnv   = "ZKA_TEST_REMOTE_ATTACH_ORIGIN_ROOT"
	remoteAttachRegressionOriginConfigEnv = "ZKA_TEST_REMOTE_ATTACH_ORIGIN_CONFIG"
)

func TestRemoteAttachRegressionSSHHelperProcess(t *testing.T) {
	if os.Getenv(remoteAttachRegressionHelperEnv) != "remote-control" {
		return
	}
	if err := os.Setenv("ZKA_CONFIG", os.Getenv(remoteAttachRegressionOriginConfigEnv)); err != nil {
		os.Exit(2)
	}
	session, err := yamux.Server(newStdioConn(os.Stdin, os.Stdout), remoteYamuxConfig())
	if err != nil {
		os.Exit(2)
	}
	defer session.Close()
	control, err := session.AcceptStream()
	if err != nil {
		os.Exit(2)
	}
	defer control.Close()
	if err := runRemoteControlSession(
		context.Background(),
		testPaths(os.Getenv(remoteAttachRegressionOriginRootEnv)),
		control,
		control,
		session,
		"",
	); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRemoteAttachImmediatelyClaimsCredentialsFromReadyAttachment(t *testing.T) {
	const host = "origin.test"
	root := testRoot(t)
	originRoot := filepath.Join(root, "origin")
	providerRoot := filepath.Join(root, "provider")

	origin, err := newTestDaemon(t, originRoot, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	provider, err := newTestDaemon(t, providerRoot, quietRunner())
	if err != nil {
		t.Fatal(err)
	}

	agentPath := filepath.Join(provider.paths.RuntimeDir, "provider-agent.sock")
	agent, err := listenUnix(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })

	bundle := sshCredentialBundle()
	origin.config.Credentials.DefaultBundle = "work"
	origin.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	origin.config.Credentials.Providers = map[string]CredentialProviderConfig{
		"provider": {NodeID: provider.state.Node.ID},
	}
	provider.config.Credentials.DefaultBundle = "work"
	provider.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	provider.config.SSH.Command = os.Args[0]
	provider.config.SSH.Options = []string{"-test.run=^TestRemoteAttachRegressionSSHHelperProcess$", "--"}
	provider.config.SSH.ExpectedNodeIDs = map[string]string{host: origin.state.Node.ID}
	provider.config.SSH.IdentityAgent = agentPath
	provider.sshAgent = newSSHAgentInfo(provider.config, agentPath)

	originConfigPath := filepath.Join(root, "origin-config.json")
	providerConfigPath := filepath.Join(root, "provider-config.json")
	writeRemoteAttachRegressionConfig(t, originConfigPath, origin.config)
	writeRemoteAttachRegressionConfig(t, providerConfigPath, provider.config)
	t.Setenv("ZKA_CONFIG", providerConfigPath)
	t.Setenv("SSH_AUTH_SOCK", agentPath)
	t.Setenv(remoteAttachRegressionHelperEnv, "remote-control")
	t.Setenv(remoteAttachRegressionOriginRootEnv, originRoot)
	t.Setenv(remoteAttachRegressionOriginConfigEnv, originConfigPath)

	serveTestDaemon(t, origin)
	serveTestDaemon(t, provider)

	plan, err := TemplateGenesis(DefaultSessionTemplate(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := origin.createWorkspace(createWorkspaceRequest{
		Name: "reviewer", Shell: []string{"fish"}, Panes: plan.Panes,
		Topology: plan.Topology, FocusPane: plan.FocusPane,
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Topology.Generation != 1 {
		t.Fatalf("genesis topology generation = %d, want 1", workspace.Topology.Generation)
	}
	pane := firstPane(workspace)
	attachmentID := localAttachmentID(provider.state.Node.ID, workspace.ID)
	if _, err := origin.registerAttachment(workspace.ID, Attachment{
		ID: attachmentID, Node: provider.state.Node,
		Transport: Transport{Kind: "ssh", Host: host},
		Endpoint:  "ssh:" + provider.state.Node.Name + ":" + attachmentID,
	}); err != nil {
		t.Fatal(err)
	}
	authoritative, err := origin.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.cacheRemoteWorkspace(host, authoritative); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.registerAttachment(workspace.ID, Attachment{
		ID: attachmentID, Node: provider.state.Node,
		Transport: Transport{Kind: "ssh", Host: host},
		Endpoint:  "unix:" + filepath.Join(provider.paths.AttachmentDir, attachmentID+".sock"),
	}); err != nil {
		t.Fatal(err)
	}
	attached, err := provider.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachmentID,
		TopologyGeneration: workspace.Topology.Generation,
		TopologyDigest:     workspace.Topology.Digest,
		ObservedTopology:   workspace.Topology.Roots,
		Status:             AttachmentReady,
		Views:              readyView(pane.ID, 72),
	})
	if err != nil {
		t.Fatal(err)
	}
	localAttachment := attached.Attachments[attachmentID]
	if localAttachment == nil || localAttachment.Status != AttachmentReady {
		t.Fatalf("local attachment before remote readiness = %#v", localAttachment)
	}

	// Model an older workspace projection racing the readiness response. The
	// attach operation already holds a generation-1 ready local attachment, but
	// the daemon temporarily projects the pre-genesis generation-0 topology.
	provider.mu.Lock()
	provider.state.Workspaces[workspace.ID].Topology = DesiredTopology{}
	provider.state.Workspaces[workspace.ID].UpdatedAt = time.Now().UTC()
	provider.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finalized, err := finalizeLaunchedWorkspaceAttach(
		ctx,
		liveWorkspaceAttachOperations{api: NewAPI(provider.paths)},
		host,
		false,
		attached,
		localAttachment,
	)
	if err != nil {
		t.Fatalf("finalize remote attachment: %v", err)
	}
	authoritative, err = origin.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := authoritative.Attachments[attachmentID].Status; got != AttachmentReady {
		t.Fatalf("origin attachment status = %q, want %q", got, AttachmentReady)
	}
	if projected := finalized.Attachments[attachmentID]; projected == nil || projected.Status == AttachmentReady {
		t.Fatalf("provider projection should remain stale before claim, got %#v", projected)
	}

	if err := claimAttachedWorkspaceCredentials(
		NewAPI(provider.paths), host, finalized, attachmentID, "work", io.Discard,
	); err != nil {
		t.Fatalf("immediate credential claim after successful remote readiness: %v", err)
	}
	authoritative, err = origin.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := authoritative.CredentialClaim
	if claim == nil || claim.State != "ready" || claim.OwnerNodeID != provider.state.Node.ID || claim.OwnerAttachmentID != attachmentID {
		t.Fatalf("origin credential claim = %#v", claim)
	}
}

func TestCredentialOwnerAttachmentExplicitSelectionDisambiguatesNodeAttachments(t *testing.T) {
	workspace := &Workspace{Attachments: map[string]*Attachment{
		"first":  {ID: "first", Node: Host{ID: "provider"}, Status: AttachmentReady},
		"second": {ID: "second", Node: Host{ID: "provider"}, Status: AttachmentReady},
	}}
	got, err := credentialOwnerAttachment(workspace, "provider", "second")
	if err != nil {
		t.Fatal(err)
	}
	if got != "second" {
		t.Fatalf("credential owner attachment = %q, want second", got)
	}
	if _, err := credentialOwnerAttachment(workspace, "provider", ""); err == nil {
		t.Fatal("implicit selection with multiple ready attachments unexpectedly succeeded")
	}
}

func writeRemoteAttachRegressionConfig(t testing.TB, path string, cfg Config) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
