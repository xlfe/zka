package zka

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceAgentClaimUsesStableRelayAndFailsClosedOnDetach(t *testing.T) {
	root := t.TempDir()
	originSocket := filepath.Join(root, "upstream", "origin.sock")
	forwardedSocket := filepath.Join(root, "upstream", "forwarded.sock")
	listenTestAgent(t, originSocket, "origin")
	listenTestAgent(t, forwardedSocket, "remote")
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"ssh":{"forward_agent":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", configPath)
	t.Setenv("SSH_AUTH_SOCK", originSocket)
	paths := testPaths(root)
	agentDir, err := os.MkdirTemp("", "zka-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	paths.AgentDir = agentDir
	t.Cleanup(func() { _ = os.RemoveAll(paths.AgentDir) })
	d, err := NewDaemon(paths, quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{
		WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42,
		AgentRelayVersion: agentRelayVersion,
	}); err != nil {
		t.Fatal(err)
	}
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "remote", Node: Host{ID: "destination", Name: "destination.example"},
		Transport: Transport{Kind: "ssh", Host: "origin.example"}, Endpoint: "ssh:destination:remote",
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
	if _, err := d.setAttachmentPaneReady(attachmentPaneReadyRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID, Ready: true, AgentSocket: forwardedSocket,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.Save(d.state); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(paths.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), forwardedSocket) {
		t.Fatal("ephemeral forwarded agent path was persisted")
	}
	status, err := d.claimWorkspaceAgent(workspaceAgentRequest{Workspace: workspace.ID, Attachment: attachment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "forwarded" || !status.Available || status.ClaimedAttachmentID != attachment.ID {
		t.Fatalf("claimed status = %#v", status)
	}
	if got := relayRoundTrip(t, d.agentRelays.path(workspace.ID)); got != "remote" {
		t.Fatalf("forwarded relay = %q", got)
	}
	if _, err := d.detachAttachment(workspace.ID, attachment.ID); err != nil {
		t.Fatal(err)
	}
	status, err = d.workspaceAgentStatus(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "disconnected" || status.Available || status.ClaimedAttachmentID != attachment.ID {
		t.Fatalf("detached status = %#v", status)
	}
	status, err = d.releaseWorkspaceAgent(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "origin" || !status.Available {
		t.Fatalf("released status = %#v", status)
	}
}

func TestWorkspaceAgentClaimRejectsLegacyPane(t *testing.T) {
	root := t.TempDir()
	forwardedSocket := filepath.Join(root, "forwarded.sock")
	listenTestAgent(t, forwardedSocket, "remote")
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"ssh":{"forward_agent":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", configPath)
	paths := testPaths(root)
	agentDir, err := os.MkdirTemp("", "zka-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	paths.AgentDir = agentDir
	t.Cleanup(func() { _ = os.RemoveAll(paths.AgentDir) })
	d, err := NewDaemon(paths, quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42}); err != nil {
		t.Fatal(err)
	}
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "remote", Node: Host{ID: "destination"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:destination:remote",
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
	if _, err := d.setAttachmentPaneReady(attachmentPaneReadyRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID, Ready: true, AgentSocket: forwardedSocket,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = d.claimWorkspaceAgent(workspaceAgentRequest{Workspace: workspace.ID, Attachment: attachment.ID})
	if err == nil || !strings.Contains(err.Error(), pane.ID) || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy claim error = %v", err)
	}
}

func TestWorkspaceAgentRelayRebindsAtSamePathAfterDaemonRestart(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"ssh":{"forward_agent":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", configPath)
	paths := testPaths(root)
	agentDir, err := os.MkdirTemp("", "zka-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	paths.AgentDir = agentDir
	t.Cleanup(func() { _ = os.RemoveAll(paths.AgentDir) })

	first, err := NewDaemon(paths, quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, first, 1)
	relayPath := first.agentRelays.path(workspace.ID)
	if _, err := os.Stat(relayPath); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(relayPath); !os.IsNotExist(err) {
		t.Fatalf("relay survived daemon close: %v", err)
	}

	second, err := NewDaemon(paths, quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(relayPath); err != nil {
		t.Fatalf("relay was not rebound at %s: %v", relayPath, err)
	}
}

func firstPane(workspace *Workspace) *Pane { return workspace.SortedPanes()[0] }

func testManifest(workspace *Workspace) Manifest {
	pane := firstPane(workspace)
	return Manifest{
		KittyVersion: "kitty 0.47.4",
		Session:      "launch --var zka_workspace=" + workspace.ID + " --var zka_pane=" + pane.ID + " -- zka pane --workspace " + workspace.ID + " --pane " + pane.ID + "\n",
		Topology:     []Node{{Kind: "os-window", Children: []Node{{Kind: "tab", Layout: "splits", Children: []Node{{Kind: "pane", PaneID: pane.ID, Title: "shell", CWD: "/work"}}}}}},
	}
}

func readyView(paneID string, id int64) map[string]RuntimeView {
	return map[string]RuntimeView{paneID: {PaneID: paneID, WindowID: id, Ready: true}}
}

func TestWorkspaceAndPaneStateTransitions(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	for _, test := range []struct {
		kind string
		want AgentState
	}{
		{"session_start", StateIdle}, {"user_prompt", StateWorking},
		{"permission_request", StateBlocked}, {"post_tool", StateWorking}, {"stop", StateDone},
	} {
		got, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: test.kind, Source: "test", TurnID: "turn-1"})
		if err != nil {
			t.Fatalf("%s: %v", test.kind, err)
		}
		if got.Panes[pane.ID].State != test.want || got.Attention != test.want {
			t.Fatalf("%s = %#v", test.kind, got)
		}
	}
	seen, err := d.markSeen(workspace.ID, pane.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Panes[pane.ID].State != StateIdle || seen.Panes[pane.ID].Evidence.Event != "seen" {
		t.Fatalf("seen = %#v", seen)
	}
}

func TestProcessFailureBecomesPaneAndWorkspaceError(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "wrapper", PID: 42}); err != nil {
		t.Fatal(err)
	}
	code := 17
	got, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_exit", Source: "wrapper", ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	failed := got.Panes[pane.ID]
	if failed.State != StateError || got.Attention != StateError || !failed.BackendCreated || failed.BackendReady {
		t.Fatalf("failed = %#v", got)
	}
}

func TestMarkSeenAcknowledgesErrorWithoutErasingDiagnosticState(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{
		WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "agent_error", Source: "codex-hook",
	}); err != nil {
		t.Fatal(err)
	}

	seen, err := d.markSeen(workspace.ID, pane.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := seen.Panes[pane.ID]
	if got.State != StateError || got.Evidence.Event != "agent_error" || got.AttentionSeen != attentionEventIdentity(got) {
		t.Fatalf("seen error lost diagnostic state: %#v", got)
	}
	if snapshot := d.attentionSnapshot(); snapshot.Counts.Total != 0 {
		t.Fatalf("seen error remained in attention: %#v", snapshot)
	}

	d.Close()
	restarted, err := NewDaemon(testPaths(root), quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if snapshot := restarted.attentionSnapshot(); snapshot.Counts.Total != 0 {
		t.Fatalf("seen error returned after restart: %#v", snapshot)
	}

	if _, err := restarted.applyEvent(context.Background(), Event{
		WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "agent_error", Source: "codex-hook",
	}); err != nil {
		t.Fatal(err)
	}
	if snapshot := restarted.attentionSnapshot(); snapshot.Counts.Error != 1 {
		t.Fatalf("new error did not return to attention: %#v", snapshot)
	}
}

func TestPreparePaneAllocatesAndNeverRestartsMissingBackend(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	first, err := d.preparePane(workspace.ID, pane.ID, "")
	if err != nil || !first.Create {
		t.Fatalf("first = %#v, %v", first, err)
	}
	second, err := d.preparePane(workspace.ID, pane.ID, "")
	if err != nil || second.Create {
		t.Fatalf("second = %#v, %v", second, err)
	}
	created, err := d.preparePane(workspace.ID, "", "/work/project")
	if err != nil || !created.Create || created.Pane.ID == pane.ID || created.Workspace.Revision != workspace.Revision+1 {
		t.Fatalf("allocated = %#v, %v", created, err)
	}
	if created.Pane.CWD != "/work/project" {
		t.Fatalf("allocated pane cwd = %q", created.Pane.CWD)
	}
}

func TestPaneAllocationKeyMakesRemoteRetryIdempotent(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	first, err := d.allocatePane(workspace.ID, "attachment:request", "/remote/project")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.allocatePane(workspace.ID, "attachment:request", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Pane.ID != second.Pane.ID || first.Workspace.Revision != second.Workspace.Revision {
		t.Fatalf("allocation replay created another pane: first=%#v second=%#v", first, second)
	}
	if first.Pane.CWD != "/remote/project" || second.Pane.CWD != "/remote/project" {
		t.Fatalf("allocation lost cwd: first=%q second=%q", first.Pane.CWD, second.Pane.CWD)
	}
}

func TestTwoPhaseMoveCommitsOnlyReadyDestination(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	source, err := d.registerAttachment(workspace.ID, Attachment{ID: "source", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/source"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: source.ID, ExpectedRevision: workspace.Revision, Manifest: testManifest(workspace), Views: readyView(pane.ID, 1)})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := d.registerAttachment(workspace.ID, Attachment{ID: "destination", Node: Host{ID: "laptop.example", Name: "laptop.example"}, Transport: Transport{Kind: "ssh", Host: "devbox.example"}, Endpoint: "ssh:laptop.example"})
	if err != nil {
		t.Fatal(err)
	}
	before := workspace.Revision
	if _, err := d.commitMove(moveCommitRequest{Workspace: workspace.ID, Destination: destination.ID, ExpectedRevision: before}); err == nil {
		t.Fatal("unready destination committed")
	}
	workspace, err = d.updateAttachment(attachmentUpdateRequest{Workspace: workspace.ID, Attachment: destination.ID, ExpectedRevision: before, Status: AttachmentReady, Views: readyView(pane.ID, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.setAttachmentPaneReady(attachmentPaneReadyRequest{Workspace: workspace.ID, Attachment: destination.ID, Pane: pane.ID, Ready: true}); err != nil {
		t.Fatal(err)
	}
	result, err := d.commitMove(moveCommitRequest{Workspace: workspace.ID, Destination: destination.ID, ExpectedRevision: workspace.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if result.Previous == nil || result.Previous.ID != source.ID || result.Workspace.PrimaryAttachmentID != destination.ID || !result.Workspace.Attachments[source.ID].Revoked {
		t.Fatalf("move = %#v", result)
	}
	if result.Workspace.Revision != before+1 {
		t.Fatalf("revision = %d", result.Workspace.Revision)
	}
	idempotent, err := d.commitMove(moveCommitRequest{Workspace: workspace.ID, Destination: destination.ID, ExpectedRevision: before})
	if err != nil || idempotent.Previous != nil || idempotent.Workspace.PrimaryAttachmentID != destination.ID {
		t.Fatalf("idempotent move = %#v, %v", idempotent, err)
	}
}

func TestManifestDoesNotBecomeReadyBeforeZMXClient(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/kitty"})
	if err != nil {
		t.Fatal(err)
	}
	views := readyView(pane.ID, 1)
	view := views[pane.ID]
	view.Ready = false
	views[pane.ID] = view
	if _, err := d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: attachment.ID, Manifest: testManifest(workspace), Views: views}); err == nil {
		t.Fatal("manifest became ready before its zmx client")
	}
	views[pane.ID] = RuntimeView{PaneID: pane.ID, WindowID: 1, Ready: true}
	if _, err := d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: attachment.ID, Manifest: testManifest(workspace), Views: views}); err != nil {
		t.Fatalf("ready manifest: %v", err)
	}
}

func TestLateCaptureCannotOverwriteDetachedWorkspaceManifest(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/kitty"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: attachment.ID, Manifest: testManifest(workspace), Views: readyView(pane.ID, 1)})
	if err != nil {
		t.Fatal(err)
	}
	good := workspace.Manifest.Session
	if _, err := d.detachAttachment(workspace.ID, attachment.ID); err != nil {
		t.Fatal(err)
	}
	late := testManifest(workspace)
	late.Session = "new_tab collapsed\n" + late.Session
	if _, err := d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: attachment.ID, Manifest: late, Views: readyView(pane.ID, 2)}); err == nil {
		t.Fatal("late detached capture succeeded")
	}
	got, _ := d.getWorkspace(workspace.ID)
	if got.Manifest.Session != good {
		t.Fatalf("manifest changed after detach: %q", got.Manifest.Session)
	}
}

func TestReopeningDetachedWorkspaceRestoresPrimaryRole(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{ID: "same", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/same"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.detachAttachment(workspace.ID, attachment.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := d.registerAttachment(workspace.ID, Attachment{ID: "same", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/same"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.getWorkspace(workspace.ID)
	if reopened.Role != AttachmentPrimary || got.PrimaryAttachmentID != reopened.ID {
		t.Fatalf("reopened=%#v workspace=%#v", reopened, got)
	}
}

func TestManifestRejectsForegroundAndUnknownPaneIndirectly(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	source, err := d.registerAttachment(workspace.ID, Attachment{ID: "source", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/source"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(workspace)
	manifest.Topology[0].Children[0].Children[0].PaneID = "missing"
	if _, err := d.updateManifest(manifestUpdateRequest{Workspace: workspace.ID, Attachment: source.ID, Manifest: manifest}); err == nil || !strings.Contains(err.Error(), "unknown pane") {
		t.Fatalf("error = %v", err)
	}
	got, _ := d.getWorkspace(workspace.ID)
	if got.Manifest.Session != "" {
		t.Fatal("invalid manifest overwrote the last good manifest")
	}
}

func TestDaemonRestartInvalidatesActiveAgentEvidence(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "user_prompt", Source: "codex"}); err != nil {
		t.Fatal(err)
	}
	restarted, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Panes[pane.ID].State != StateUnknown || got.Panes[pane.ID].Evidence.Event != "daemon_restart" {
		t.Fatalf("restart = %#v", got)
	}
}

func TestDaemonCloseWaitsForWorkersAndRemovesSockets(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.paths.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon socket remains: %v", err)
	}
	if _, err := os.Stat(d.paths.WatcherSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("watcher socket remains: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := d.Start(); err == nil {
		t.Fatal("closed daemon restarted")
	}
}

func TestRemoveStaleSocketRefusesRegularFile(t *testing.T) {
	path := t.TempDir() + "/socket"
	if err := os.WriteFile(path, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleSocket(path); err == nil {
		t.Fatal("regular file was removed")
	}
}

func TestUnknownEventIsRejected(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	_, err = d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: firstPane(workspace).ID, Kind: "surprise", Source: "test"})
	if err == nil || !strings.Contains(err.Error(), "unsupported event") {
		t.Fatalf("error = %v", err)
	}
}
