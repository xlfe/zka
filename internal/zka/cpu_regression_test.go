package zka

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

func TestKittyStateProjectionUsesOnlyReadyLocalAttachments(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	workspace.RemoteHost = "devbox.example"
	localNode := d.state.Node
	workspace.Attachments = map[string]*Attachment{
		"laptop": {
			ID: "laptop", Node: localNode, Endpoint: "unix:/laptop.sock", Status: AttachmentReady,
			Views: readyView(pane.ID, 1),
		},
		"devbox": {
			ID: "devbox", Node: Host{ID: "devbox"}, Endpoint: "unix:/devbox.sock", Status: AttachmentReady,
			Views: readyView(pane.ID, 2),
		},
		"detached": {
			ID: "detached", Node: localNode, Endpoint: "unix:/detached.sock", Status: AttachmentDetached,
			Views: readyView(pane.ID, 3),
		},
		"revoked": {
			ID: "revoked", Node: localNode, Endpoint: "unix:/revoked.sock", Status: AttachmentReady, Revoked: true,
			Views: readyView(pane.ID, 4),
		},
	}

	d.updateKittyState(context.Background(), workspace)

	var stateUpdates int
	stateCalls := runner.Calls()
	// set-user-vars + set-window-title for the one ready local attachment.
	// Tab titles are diffed against the desired topology, so a workspace with
	// none issues no extra `ls`.
	if len(stateCalls) != 2 {
		t.Fatalf("local Kitty projection commands = %d, want 2", len(stateCalls))
	}
	for _, call := range stateCalls {
		if call.Name != "kitten" {
			continue
		}
		joined := strings.Join(call.Args, " ")
		if !strings.Contains(joined, "--to unix:/laptop.sock") {
			t.Fatalf("Kitty command targeted an ineligible attachment: %#v", call.Args)
		}
		if strings.Contains(joined, " set-user-vars ") {
			stateUpdates++
		}
	}
	if stateUpdates != 1 {
		t.Fatalf("local pane state updates = %d, want 1", stateUpdates)
	}

	// Withdrawal no longer consults attachments at all: the notifier's registry
	// is the authority on what was actually posted, so this issues no Kitty
	// remote-control command and exactly one withdrawal for the named pane.
	notificationRunner := quietRunner()
	d.kitty.Runner = notificationRunner
	d.closeDesktopNotifications(context.Background(), workspace, pane.ID)
	if calls := notificationRunner.Calls(); len(calls) != 0 {
		t.Fatalf("notification withdrawal issued Kitty commands: %#v", calls)
	}
	want := []paneRef{{Workspace: workspace.ID, Pane: pane.ID}}
	if got := fakeDesktop(t, d).Withdrawn(); !reflect.DeepEqual(got, want) {
		t.Fatalf("withdrawn = %#v, want %#v", got, want)
	}
}

func TestKittyStateProjectionDoesNothingWithoutLocalAttachment(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	workspace.RemoteHost = "devbox.example"
	workspace.Attachments = map[string]*Attachment{
		"devbox": {
			ID: "devbox", Node: Host{ID: "devbox"}, Endpoint: "unix:/devbox.sock", Status: AttachmentReady,
			Views: readyView(pane.ID, 2),
		},
	}

	d.updateKittyState(context.Background(), workspace)

	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("Kitty commands without a local attachment = %#v", calls)
	}
}

func TestEndpointLookupIgnoresNonLocalAttachments(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	d.mu.Lock()
	actual := d.state.Workspaces[workspace.ID]
	actual.Attachments["devbox"] = &Attachment{
		ID: "devbox", Node: Host{ID: "devbox"}, Endpoint: "unix:/shared-path.sock", Status: AttachmentReady,
	}
	d.mu.Unlock()

	if foundWorkspace, foundAttachment := d.endpointAttachment("unix:/shared-path.sock"); foundWorkspace != nil || foundAttachment != nil {
		t.Fatalf("foreign endpoint resolved locally: workspace=%#v attachment=%#v", foundWorkspace, foundAttachment)
	}

	d.mu.Lock()
	actual.Attachments["laptop"] = &Attachment{
		ID: "laptop", Node: d.state.Node, Endpoint: "unix:/shared-path.sock", Status: AttachmentReady,
	}
	d.mu.Unlock()
	foundWorkspace, foundAttachment := d.endpointAttachment("unix:/shared-path.sock")
	if foundWorkspace == nil || foundAttachment == nil || foundAttachment.ID != "laptop" {
		t.Fatalf("local endpoint lookup = workspace=%#v attachment=%#v", foundWorkspace, foundAttachment)
	}
}

func TestRemoteSnapshotIdempotenceAndSingleLocalPaneUpdate(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.DesktopEnabled = false
	d.config.Notifications.NtfyEnabled = false
	now := time.Now().UTC()
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "remote-workspace",
		Origin: Host{ID: "devbox", Name: "devbox"}, Revision: 1,
		Panes: map[string]*Pane{
			"pane": {
				ID: "pane", Title: "shell", Phase: PaneAdmitted, State: StateWorking,
				CreatedAt: now, UpdatedAt: now,
			},
		},
		Attachments: map[string]*Attachment{
			"devbox": {
				ID: "devbox", Node: Host{ID: "devbox"}, Endpoint: "unix:/devbox.sock",
				Status: AttachmentReady, Views: readyView("pane", 2),
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["laptop"] = &Attachment{
		ID: "laptop", Node: d.state.Node, Endpoint: "unix:/laptop.sock",
		Status: AttachmentReady, Views: readyView("pane", 1),
	}
	d.mu.Unlock()

	if _, err := d.cacheRemoteWorkspace("devbox.example", remote.Clone()); err != nil {
		t.Fatal(err)
	}
	d.waitBackground()
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("identical remote snapshot invoked Kitty: %#v", calls)
	}

	changed := remote.Clone()
	changed.Panes["pane"].State = StateBlocked
	changed.Panes["pane"].UpdatedAt = now.Add(time.Second)
	changed.UpdatedAt = changed.Panes["pane"].UpdatedAt
	if _, err := d.cacheRemoteWorkspace("devbox.example", changed); err != nil {
		t.Fatal(err)
	}
	d.waitBackground()

	var stateUpdates int
	changeCalls := runner.Calls()
	if len(changeCalls) != 2 {
		t.Fatalf("Kitty commands after one real remote change = %d, want 2", len(changeCalls))
	}
	for _, call := range changeCalls {
		if call.Name != "kitten" {
			continue
		}
		joined := strings.Join(call.Args, " ")
		if !strings.Contains(joined, "--to unix:/laptop.sock") {
			t.Fatalf("remote pane change targeted a non-local attachment: %#v", call.Args)
		}
		if strings.Contains(joined, " set-user-vars ") {
			stateUpdates++
		}
	}
	if stateUpdates != 1 {
		t.Fatalf("local pane state updates after one real change = %d, want 1", stateUpdates)
	}
}

func TestRemoteStreamDoesNotReplayInitialSnapshotAsWorkspaceEvents(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	createTestWorkspace(t, d, 1)
	serveTestDaemon(t, d)

	var output synchronizedBuffer
	writer := &remoteControlWriter{enc: json.NewEncoder(&output)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamRemoteEvents(ctx, NewAPI(d.paths), writer)
	}()
	waitFor(t, func() bool { return strings.Contains(output.String(), `"op":"snapshot"`) })
	cancel()
	<-done

	messages := strings.TrimSpace(output.String())
	if strings.Count(messages, "\n") != 0 || strings.Contains(messages, `"op":"workspace"`) {
		t.Fatalf("initial remote snapshot was followed by duplicate workspace events: %s", messages)
	}
}

func TestUnchangedManifestCaptureDoesNotAdvanceSemanticTimestamps(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/laptop.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: testManifest(workspace),
		Views: map[string]RuntimeView{
			pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true, LastSeen: time.Now().UTC()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedManifest := first.Manifest
	replayedManifest.CapturedAt = first.Manifest.CapturedAt.Add(time.Minute)
	second, err := d.updateManifest(manifestUpdateRequest{
		Workspace: first.ID, Attachment: attachment.ID, ExpectedRevision: first.Revision,
		Manifest: replayedManifest,
		Views: map[string]RuntimeView{
			pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true, LastSeen: time.Now().UTC().Add(time.Minute)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("workspace semantic timestamp advanced: %s -> %s", first.UpdatedAt, second.UpdatedAt)
	}
	if remoteWorkspaceFingerprint(second) != remoteWorkspaceFingerprint(first) {
		t.Fatalf("unchanged capture changed remote fingerprint: %q -> %q",
			remoteWorkspaceFingerprint(first), remoteWorkspaceFingerprint(second))
	}
	if !second.Manifest.CapturedAt.Equal(first.Manifest.CapturedAt) {
		t.Fatalf("manifest capture timestamp advanced: %s -> %s", first.Manifest.CapturedAt, second.Manifest.CapturedAt)
	}
	if !second.Attachments[attachment.ID].UpdatedAt.Equal(first.Attachments[attachment.ID].UpdatedAt) {
		t.Fatalf("attachment semantic timestamp advanced: %s -> %s",
			first.Attachments[attachment.ID].UpdatedAt, second.Attachments[attachment.ID].UpdatedAt)
	}

	changedManifest := second.Manifest
	changedManifest.Topology = cloneNodes(second.Manifest.Topology)
	changedManifest.Topology[0].Children[0].Children[0].Title = "renamed"
	third, err := d.updateManifest(manifestUpdateRequest{
		Workspace: second.ID, Attachment: attachment.ID, ExpectedRevision: second.Revision,
		Manifest: changedManifest, Views: second.Attachments[attachment.ID].Views,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A pane title is presentation, so it must be recorded without advancing
	// the structural revision -- otherwise every title change would invalidate
	// every other attachment and force a rebuild.
	if !third.UpdatedAt.After(second.UpdatedAt) {
		t.Fatalf("real manifest change was suppressed: updated %s -> %s", second.UpdatedAt, third.UpdatedAt)
	}
	if third.Revision != second.Revision || third.Topology.Generation != second.Topology.Generation {
		t.Fatalf("presentation change advanced structural identity: revision %d -> %d, generation %d -> %d",
			second.Revision, third.Revision, second.Topology.Generation, third.Topology.Generation)
	}

	// A structural change must advance both.
	structural := second.Manifest
	structural.Topology = cloneNodes(second.Manifest.Topology)
	structural.Topology[0].Children = append(structural.Topology[0].Children, Node{
		Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "unknown-pane"}},
	})
	if _, err := d.updateManifest(manifestUpdateRequest{
		Workspace: second.ID, Attachment: attachment.ID, ExpectedRevision: third.Revision,
		Manifest: structural, Views: third.Attachments[attachment.ID].Views,
	}); err == nil {
		t.Fatal("a capture referencing an unknown pane was accepted")
	}
}

func TestUnchangedMirrorCaptureDoesNotAdvanceSemanticTimestamps(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "mirror", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/laptop.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Status: AttachmentReady,
		Views: map[string]RuntimeView{
			pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true, LastSeen: time.Now().UTC()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.updateAttachment(attachmentUpdateRequest{
		Workspace: first.ID, Attachment: attachment.ID, ExpectedRevision: first.Revision, Status: AttachmentReady,
		Views: map[string]RuntimeView{
			pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true, LastSeen: time.Now().UTC().Add(time.Minute)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("workspace semantic timestamp advanced: %s -> %s", first.UpdatedAt, second.UpdatedAt)
	}
	if !second.Attachments[attachment.ID].UpdatedAt.Equal(first.Attachments[attachment.ID].UpdatedAt) {
		t.Fatalf("attachment semantic timestamp advanced: %s -> %s",
			first.Attachments[attachment.ID].UpdatedAt, second.Attachments[attachment.ID].UpdatedAt)
	}

	changedView := second.Attachments[attachment.ID].Views[pane.ID]
	changedView.Focused = true
	third, err := d.updateAttachment(attachmentUpdateRequest{
		Workspace: second.ID, Attachment: attachment.ID, ExpectedRevision: second.Revision, Status: AttachmentReady,
		Views: map[string]RuntimeView{pane.ID: changedView},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !third.UpdatedAt.After(second.UpdatedAt) ||
		!third.Attachments[attachment.ID].UpdatedAt.After(second.Attachments[attachment.ID].UpdatedAt) {
		t.Fatalf("real mirror view change was suppressed: workspace %s -> %s, attachment %s -> %s",
			second.UpdatedAt, third.UpdatedAt,
			second.Attachments[attachment.ID].UpdatedAt, third.Attachments[attachment.ID].UpdatedAt)
	}
}
