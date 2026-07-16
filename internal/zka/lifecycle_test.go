package zka

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecycleRunner struct {
	mu       sync.Mutex
	sessions map[string]bool
	failKill bool
	calls    []runnerCall
}

func newLifecycleRunner(names ...string) *lifecycleRunner {
	runner := &lifecycleRunner{sessions: map[string]bool{}}
	for _, name := range names {
		runner.sessions[name] = true
	}
	return runner
}

func (r *lifecycleRunner) setSession(name string, exists bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if exists {
		r.sessions[name] = true
	} else {
		delete(r.sessions, name)
	}
}

func (r *lifecycleRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runnerCall{Name: name, Args: append([]string(nil), args...)})
	if name == "kitten" {
		if strings.Join(args, " ") == "--version" {
			return "kitten 0.47.4\n", "", nil
		}
		return "", "", nil
	}
	if len(args) >= 2 && args[0] == "list" && args[1] == "--short" {
		var names []string
		for session := range r.sessions {
			names = append(names, session)
		}
		sort.Strings(names)
		return strings.Join(names, "\n") + "\n", "", nil
	}
	if len(args) >= 2 && args[0] == "kill" {
		if r.failKill {
			return "", "busy", fmt.Errorf("kill failed")
		}
		delete(r.sessions, args[1])
		return "", "", nil
	}
	return "", "", nil
}

func manifestForPanes(workspace *Workspace, paneIDs ...string) Manifest {
	children := make([]Node, 0, len(paneIDs))
	var session strings.Builder
	for _, paneID := range paneIDs {
		pane := workspace.Panes[paneID]
		children = append(children, Node{Kind: "pane", PaneID: paneID, Title: pane.Title, CWD: pane.CWD})
		fmt.Fprintf(&session, "launch --var zka_workspace=%s --var zka_pane=%s -- zka pane --workspace %s --pane %s\n", workspace.ID, paneID, workspace.ID, paneID)
	}
	return Manifest{
		KittyVersion: "kitty 0.47.4", Session: session.String(),
		Topology: []Node{{Kind: "os-window", Children: []Node{{Kind: "tab", Layout: "splits", Children: children}}}},
	}
}

func viewsForPanes(paneIDs ...string) map[string]RuntimeView {
	views := map[string]RuntimeView{}
	for index, paneID := range paneIDs {
		views[paneID] = RuntimeView{PaneID: paneID, WindowID: int64(index + 1), Ready: true}
	}
	return views
}

func TestBackendReconcileKeepsPartialWorkspaceAndMarksDeadPane(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	for _, pane := range panes {
		if _, err := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner.setSession(panes[0].Backend.Ref, true)

	result, err := d.reconcileBackends(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 || len(result.Marked) != 1 || result.Marked[0] != panes[1].ID {
		t.Fatalf("reconcile result = %#v", result)
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Panes[panes[0].ID].BackendDead || !got.Panes[panes[0].ID].BackendReady {
		t.Fatalf("live pane was marked dead: %#v", got.Panes[panes[0].ID])
	}
	dead := got.Panes[panes[1].ID]
	if !dead.BackendDead || dead.BackendReady || dead.Process.Running || dead.State != StateError || dead.Evidence.Event != "backend_missing" {
		t.Fatalf("missing pane was not tombstoned: %#v", dead)
	}
}

func TestBackendReconcileDeletesWorkspaceWhenAllBackendsAreDead(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	for _, pane := range workspace.SortedPanes() {
		if _, err := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42,
		}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := d.reconcileBackends(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != workspace.ID {
		t.Fatalf("reconcile result = %#v", result)
	}
	if _, err := d.getWorkspace(workspace.ID); err == nil {
		t.Fatal("all-dead workspace remained in daemon state")
	}
	if _, err := os.Stat(filepath.Join(d.paths.GeneratedDir, workspace.ID)); !os.IsNotExist(err) {
		t.Fatalf("generated workspace directory still exists: %v", err)
	}
}

func TestBackendReconcileDoesNotDeleteUnstartedWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), newLifecycleRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	result, err := d.reconcileBackends(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 || len(result.Marked) != 0 {
		t.Fatalf("unstarted workspace was reconciled as dead: %#v", result)
	}
	if _, err := d.getWorkspace(workspace.ID); err != nil {
		t.Fatalf("unstarted workspace was removed: %v", err)
	}
}

func TestBackendReconcileDeletesExpiredUnstartedWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), newLifecycleRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	d.mu.Lock()
	pane := d.state.Workspaces[workspace.ID].Panes[firstPane(workspace).ID]
	pane.CreatedAt = time.Now().Add(-backendStartupGrace - time.Second)
	pane.UpdatedAt = pane.CreatedAt
	d.mu.Unlock()

	result, err := d.reconcileBackends(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != workspace.ID {
		t.Fatalf("expired unstarted workspace was retained: %#v", result)
	}
}

func readyWorkspaceAttachment(t *testing.T, d *Daemon, workspace *Workspace, id string) (*Workspace, *Attachment) {
	t.Helper()
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: id, Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	var paneIDs []string
	for _, pane := range workspace.SortedPanes() {
		paneIDs = append(paneIDs, pane.ID)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Manifest: manifestForPanes(workspace, paneIDs...), Views: viewsForPanes(paneIDs...),
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspace, workspace.Attachments[id]
}

func TestRenameWorkspaceKeepsStableIdentityAndBackends(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	renamed, err := d.renameWorkspace(renameWorkspaceRequest{Workspace: workspace.ID, Name: "  shell-work  ", ExpectedRevision: workspace.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "shell-work" || renamed.ID != workspace.ID || renamed.Panes[pane.ID].Backend.Ref != pane.Backend.Ref {
		t.Fatalf("renamed workspace = %#v", renamed)
	}
	unchanged, err := d.renameWorkspace(renameWorkspaceRequest{Workspace: workspace.ID, Name: "shell-work", ExpectedRevision: renamed.Revision})
	if err != nil || unchanged.Revision != renamed.Revision {
		t.Fatalf("same-name rename = %#v, %v", unchanged, err)
	}
	if _, err := d.createWorkspace(createWorkspaceRequest{Name: "other", Shell: []string{"fish"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.renameWorkspace(renameWorkspaceRequest{Workspace: "other", Name: "shell-work"}); err == nil {
		t.Fatal("duplicate workspace name was accepted")
	}
}

func TestMirrorPaneClosureKillsOnlyClosedBackend(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, newLifecycleRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	runner := newLifecycleRunner(panes[0].Backend.Ref, panes[1].Backend.Ref)
	d.runner = runner
	d.kitty.Runner = runner
	workspace, _ = readyWorkspaceAttachment(t, d, workspace, "primary")
	mirror, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "mirror", Node: Host{ID: "mirror-node", Name: "mirror"}, Transport: Transport{Kind: "ssh", Host: "mirror"}, Endpoint: "ssh:mirror",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: mirror.ID, ExpectedRevision: workspace.Revision,
		Status: AttachmentReady, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := d.closePanes(context.Background(), closePanesRequest{
		Workspace: workspace.ID, Attachment: mirror.ID, ExpectedRevision: workspace.Revision,
		Panes: []string{panes[0].ID}, Manifest: manifestForPanes(workspace, panes[1].ID), Views: viewsForPanes(panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Panes[panes[0].ID] != nil || updated.Panes[panes[1].ID] == nil {
		t.Fatalf("panes after close = %#v", updated.Panes)
	}
	runner.mu.Lock()
	closedExists, remainingExists := runner.sessions[panes[0].Backend.Ref], runner.sessions[panes[1].Backend.Ref]
	runner.mu.Unlock()
	if closedExists || !remainingExists {
		t.Fatalf("zmx sessions after close: closed=%v remaining=%v", closedExists, remainingExists)
	}
}

func TestStalePaneClosureDoesNotSignalZMX(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	runner := newLifecycleRunner(panes[0].Backend.Ref, panes[1].Backend.Ref)
	d.runner = runner
	d.kitty.Runner = runner
	_, err = d.closePanes(context.Background(), closePanesRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision - 1,
		Panes: []string{panes[0].ID}, Manifest: manifestForPanes(workspace, panes[1].ID), Views: viewsForPanes(panes[1].ID),
	})
	if err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatalf("stale close error = %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) != 0 && call.Args[0] == "kill" {
			t.Fatalf("stale closure signalled zmx: %#v", call)
		}
	}
}

func TestWorkspaceKillRemovesStateSessionsAndViews(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	runner := newLifecycleRunner(panes[0].Backend.Ref, panes[1].Backend.Ref)
	d.runner = runner
	d.kitty.Runner = runner
	workspace, _ = readyWorkspaceAttachment(t, d, workspace, "local")
	if _, err := d.store.WriteSession(workspace.ID, "local", "launch\n"); err != nil {
		t.Fatal(err)
	}
	response, err := d.killWorkspace(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.DeletedWorkspaceID != workspace.ID || response.Name != workspace.Name {
		t.Fatalf("kill response = %#v", response)
	}
	if _, err := d.getWorkspace(workspace.ID); err == nil {
		t.Fatal("workspace remained after kill")
	}
	if _, pending, err := d.beginWorkspaceDeletion(workspace.ID); err != nil || pending {
		t.Fatalf("idempotent kill lookup: pending=%v err=%v", pending, err)
	}
	matches, globErr := filepath.Glob(filepath.Join(d.paths.GeneratedDir, shortID(workspace.ID)+"*.kitty-session"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("generated sessions remain: %v, %v", matches, globErr)
	}
}

func TestKillFailurePersistsAndRestartResumesCleanup(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	runner := newLifecycleRunner(pane.Backend.Ref)
	runner.failKill = true
	d.runner = runner
	d.kitty.Runner = runner
	if _, _, err := d.beginWorkspaceDeletion(workspace.ID); err != nil {
		t.Fatal(err)
	}
	if d.cleanupWorkspaceOnce(context.Background(), workspace.ID) {
		t.Fatal("failed zmx kill completed workspace deletion")
	}
	pending, err := d.getWorkspace(workspace.ID)
	if err != nil || !pending.DeletionPending || pending.DeletionError == "" {
		t.Fatalf("pending workspace = %#v, %v", pending, err)
	}
	runner.failKill = false
	restarted, err := newTestDaemon(t, root, runner)
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, restarted)
	waitFor(t, func() bool {
		_, getErr := restarted.getWorkspace(workspace.ID)
		return getErr != nil
	})
}

func TestPaneRemovalFailurePersistsAndRestartResumesCleanup(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	runner := newLifecycleRunner(panes[0].Backend.Ref, panes[1].Backend.Ref)
	runner.failKill = true
	d.runner = runner
	d.kitty.Runner = runner
	if _, _, err := d.beginPaneClosure(closePanesRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Panes: []string{panes[0].ID}, Manifest: manifestForPanes(workspace, panes[1].ID), Views: viewsForPanes(panes[1].ID),
	}); err != nil {
		t.Fatal(err)
	}
	if d.cleanupWorkspaceOnce(context.Background(), workspace.ID) {
		t.Fatal("failed zmx kill completed pane removal")
	}
	pending, err := d.getWorkspace(workspace.ID)
	if err != nil || !pending.Panes[panes[0].ID].RemovalPending || pending.Panes[panes[0].ID].RemovalError == "" {
		t.Fatalf("pending pane = %#v, %v", pending, err)
	}
	runner.failKill = false
	restarted, err := newTestDaemon(t, root, runner)
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, restarted)
	waitFor(t, func() bool {
		got, getErr := restarted.getWorkspace(workspace.ID)
		return getErr == nil && got.Panes[panes[0].ID] == nil && got.Panes[panes[1].ID] != nil
	})
}

func TestFinalPaneClosureDeletesWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	if _, err := d.closePanes(context.Background(), closePanesRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Panes: []string{pane.ID}, Views: map[string]RuntimeView{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.getWorkspace(workspace.ID); err == nil {
		t.Fatal("workspace remained after its final pane closed")
	}
}

func TestDeletionPendingRejectsMutations(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	if _, _, err := d.beginWorkspaceDeletion(workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.renameWorkspace(renameWorkspaceRequest{Workspace: workspace.ID, Name: "new"}); err == nil {
		t.Fatal("rename succeeded while deletion was pending")
	}
	if _, err := d.allocatePane(workspace.ID, "new", ""); err == nil {
		t.Fatal("pane allocation succeeded while deletion was pending")
	}
	if _, err := d.detachAttachment(workspace.ID, "missing"); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("detach error = %v", err)
	}
}

func TestRollbackDeleteRejectsAttachmentsAndStartedBackends(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	if _, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/local",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.deleteWorkspace(workspace.ID); err == nil {
		t.Fatal("rollback delete accepted a workspace with an attachment")
	}

	started := createTestWorkspaceNamed(t, d, "started")
	d.mu.Lock()
	d.state.Workspaces[started.ID].Panes[firstPane(started).ID].BackendCreated = true
	d.mu.Unlock()
	if err := d.deleteWorkspace(started.ID); err == nil {
		t.Fatal("rollback delete accepted a workspace with a started backend")
	}
}

func TestV2ResetDoesNotKillLegacyZMXSessions(t *testing.T) {
	root := t.TempDir()
	paths := testPaths(root)
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"schema_version":2,"workspaces":{"old":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newLifecycleRunner("zka-old-pane")
	if _, err := newTestDaemon(t, root, runner); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if !runner.sessions["zka-old-pane"] {
		t.Fatal("v2 reset killed a legacy zmx session")
	}
	for _, call := range runner.calls {
		if len(call.Args) != 0 && call.Args[0] == "kill" {
			t.Fatalf("v2 reset invoked zmx kill: %#v", call)
		}
	}
}

func TestConfirmedQuitDeletesButDetachPreserves(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	serveTestDaemon(t, d)
	d.events <- WatcherEvent{Version: 1, Endpoint: attachment.Endpoint, Kind: "quit"}
	time.Sleep(100 * time.Millisecond)
	if _, err := d.getWorkspace(workspace.ID); err != nil {
		t.Fatalf("unconfirmed quit removed workspace: %v", err)
	}
	if _, err := d.detachAttachment(workspace.ID, attachment.ID); err != nil {
		t.Fatal(err)
	}
	d.events <- WatcherEvent{Version: 1, Endpoint: attachment.Endpoint, Kind: "quit", Confirmed: true}
	time.Sleep(100 * time.Millisecond)
	if _, err := d.getWorkspace(workspace.ID); err != nil {
		t.Fatalf("detached workspace was removed: %v", err)
	}

	second := createTestWorkspaceNamed(t, d, "second")
	second, secondAttachment := readyWorkspaceAttachment(t, d, second, "second-local")
	d.events <- WatcherEvent{Version: 1, Endpoint: secondAttachment.Endpoint, Kind: "quit", Confirmed: true}
	waitFor(t, func() bool {
		_, getErr := d.getWorkspace(second.ID)
		return getErr != nil
	})
}

func createTestWorkspaceNamed(t testing.TB, daemon *Daemon, name string) *Workspace {
	t.Helper()
	workspace, err := daemon.createWorkspace(createWorkspaceRequest{Name: name, Shell: []string{"fish"}, Panes: []PaneSpec{{CWD: "/work", Title: "pane"}}})
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}
