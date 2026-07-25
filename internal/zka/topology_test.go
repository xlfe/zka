package zka

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestV4MigrationRecoversLivePaneMissingFromManifest(t *testing.T) {
	paths := testPaths(t.TempDir())
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := StateData{
		SchemaVersion: 4,
		Node:          Host{ID: "origin", Name: "devbox"},
		Workspaces: map[string]*Workspace{
			"workspace": {
				ID: "workspace", Name: "work", Revision: 9,
				Panes: map[string]*Pane{
					"visible": {
						ID: "visible", Position: 0, Visible: true,
						Backend: BackendRef{Kind: "zmx", Ref: "visible"}, CreatedAt: now, UpdatedAt: now,
					},
					"orphan": {
						ID: "orphan", Position: 1, Visible: false, BackendCreated: true, BackendReady: true,
						Backend: BackendRef{Kind: "zmx", Ref: "orphan"}, CreatedAt: now, UpdatedAt: now,
					},
				},
				Manifest: Manifest{
					Session: "launch --var zka_workspace=workspace --var zka_pane=visible\n",
					Topology: []Node{{Kind: "os-window", Children: []Node{{
						Kind:        "tab",
						LayoutState: json.RawMessage(`{"pairs":{"one":77},"all_windows":{"window_groups":[{"id":77,"window_ids":[88]}]}}`),
						Children:    []Node{{Kind: "pane", PaneID: "visible"}},
					}}}},
				},
				Attachments: map[string]*Attachment{
					"local": {ID: "local", AppliedRevision: 9, Status: AttachmentReady},
				},
			},
		},
		Remotes: map[string]*RemoteCache{},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	workspace := loaded.Workspaces["workspace"]
	if loaded.SchemaVersion != stateSchemaVersion || workspace.Topology.Generation != 1 {
		t.Fatalf("migrated topology = %#v", workspace.Topology)
	}
	if got := topologyPaneIDs(workspace.Topology.Roots); !samePaneSet(got, map[string]bool{"visible": true, "orphan": true}) {
		t.Fatalf("migrated panes = %#v", got)
	}
	if !workspace.Panes["orphan"].Visible {
		t.Fatal("orphan pane remained hidden")
	}
	if len(workspace.Topology.Roots[0].Children[0].LayoutState) != 0 {
		t.Fatalf("migration retained attachment-local layout ids: %s", workspace.Topology.Roots[0].Children[0].LayoutState)
	}
	recovered := false
	for _, tab := range workspace.Topology.Roots[0].Children {
		if len(tab.Children) == 1 && tab.Children[0].PaneID == "orphan" && tab.Title == "Recovered orphan" {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("orphan was not placed in a recovery tab: %#v", workspace.Topology.Roots)
	}
	attachment := workspace.Attachments["local"]
	if attachment.AppliedTopologyGeneration != 0 || attachment.ReconcileStatus != "pending" {
		t.Fatalf("migration trusted stale attachment: %#v", attachment)
	}
	backup, err := os.ReadFile(paths.StateFile + ".v4.backup")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(encoded) {
		t.Fatal("v4 migration backup does not match the original state")
	}
}

func TestPartialCaptureCannotHideActivePane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "source", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "ssh:source",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifestForPanes(workspace, panes[0].ID),
		Views:    viewsForPanes(panes[0].ID),
	})
	if err == nil {
		t.Fatal("partial capture was accepted")
	}
	got, _ := d.getWorkspace(workspace.ID)
	if got.Topology.Generation != 0 || !got.Panes[panes[0].ID].Visible || !got.Panes[panes[1].ID].Visible {
		t.Fatalf("partial capture mutated canonical pane visibility: %#v", got)
	}
}

func TestMirrorCaptureCommitsPendingPaneToCanonicalTopology(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	first := firstPane(workspace)
	source, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "source", Node: Host{ID: "source"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:source",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: source.ID,
		Manifest: manifestForPanes(workspace, first.ID), Views: viewsForPanes(first.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	baseGeneration := workspace.Topology.Generation
	allocated, err := d.allocatePane(workspace.ID, "mirror:add", "/remote/work")
	if err != nil {
		t.Fatal(err)
	}
	second := allocated.Pane
	if !second.TopologyPending {
		t.Fatal("new pane was canonical before a ready capture")
	}
	mirror, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "mirror", Node: Host{ID: "mirror"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:mirror",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: mirror.ID,
		ExpectedRevision: workspace.Revision, BaseTopologyGeneration: baseGeneration,
		Manifest: manifestForPanes(workspace, first.ID, second.ID),
		Views:    viewsForPanes(first.ID, second.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Topology.Generation != baseGeneration+1 ||
		!samePaneSet(desiredPaneIDs(workspace), map[string]bool{first.ID: true, second.ID: true}) {
		t.Fatalf("mirror topology was not committed: %#v", workspace.Topology)
	}
	if workspace.Panes[second.ID].TopologyPending {
		t.Fatal("committed pane remained topology-pending")
	}
	if workspace.Attachments[source.ID].ReconcileStatus != "pending" {
		t.Fatalf("other attachment was not invalidated: %#v", workspace.Attachments[source.ID])
	}
}

func TestReadyAttachmentRequiresVerifiedTopologyDigest(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	source, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "source", Node: Host{ID: "source"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:source",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: source.ID,
		Manifest: manifestForPanes(workspace, pane.ID), Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "mirror", Node: Host{ID: "mirror"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:mirror",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: mirror.ID, Status: AttachmentReady,
		Views: viewsForPanes(pane.ID),
	})
	if err == nil {
		t.Fatal("attachment became ready without a verified topology digest")
	}
	got, _ := d.getWorkspace(workspace.ID)
	if got.Attachments[mirror.ID].Status == AttachmentReady {
		t.Fatalf("unverified attachment is ready: %#v", got.Attachments[mirror.ID])
	}
}

func TestTopologyDigestIgnoresRuntimeLayoutAndFocusIDs(t *testing.T) {
	leftLayout, err := logicalKittyLayoutState(kittyTab{
		LayoutState: json.RawMessage(`{"pairs":{"one":2},"opts":{"default_axis_is_horizontal":true},"class":"Splits","all_windows":{"active_group_idx":0,"active_group_history":[2],"window_groups":[{"id":2,"window_ids":[7]}]}}`),
		Windows:     []kittyWindow{{ID: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rightLayout, err := logicalKittyLayoutState(kittyTab{
		LayoutState: json.RawMessage(`{"all_windows":{"window_groups":[{"id":99,"window_ids":[101]}],"active_group_idx":4,"active_group_history":[99]},"class":"Splits","opts":{"default_axis_is_horizontal":true},"pairs":{"one":99}}`),
		Windows:     []kittyWindow{{ID: 101}},
	})
	if err != nil {
		t.Fatal(err)
	}
	left := []Node{{
		ID: "os", Kind: "os-window", State: "maximized", Focused: true,
		Children: []Node{{
			ID: "tab", Kind: "tab", Active: true,
			LayoutState: leftLayout,
			Children:    []Node{{ID: "pane", Kind: "pane", PaneID: "pane", CWD: "/origin", Focused: true}},
		}},
	}}
	right := cloneNodes(left)
	right[0].State = "normal"
	right[0].Focused = false
	right[0].Children[0].Active = false
	right[0].Children[0].LayoutState = rightLayout
	right[0].Children[0].Children[0].CWD = "/destination"
	if topologyDigest(left) != topologyDigest(right) {
		t.Fatalf("runtime-local layout state changed logical digest:\n%s\n%s", topologyDigest(left), topologyDigest(right))
	}
}

func TestConcurrentMirrorAddsAreRebasedWithoutDroppingEitherPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	original := firstPane(workspace)
	firstAttachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "first", Node: Host{ID: "first"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:first",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: firstAttachment.ID,
		Manifest: manifestForPanes(workspace, original.ID), Views: viewsForPanes(original.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	baseGeneration := workspace.Topology.Generation
	secondAttachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "second", Node: Host{ID: "second"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:second",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: secondAttachment.ID,
		TopologyGeneration: baseGeneration, TopologyDigest: workspace.Topology.Digest,
		ObservedTopology: workspace.Topology.Roots,
		Status:           AttachmentReady, Views: viewsForPanes(original.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstAdd, err := d.allocatePane(workspace.ID, "first:add", "")
	if err != nil {
		t.Fatal(err)
	}
	secondAdd, err := d.allocatePane(workspace.ID, "second:add", "")
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ = d.getWorkspace(workspace.ID)
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: firstAttachment.ID,
		BaseTopologyGeneration: baseGeneration,
		Manifest:               manifestForPanes(workspace, original.ID, firstAdd.Pane.ID),
		Views:                  viewsForPanes(original.ID, firstAdd.Pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: secondAttachment.ID,
		BaseTopologyGeneration: baseGeneration,
		Manifest:               manifestForPanes(workspace, original.ID, secondAdd.Pane.ID),
		Views:                  viewsForPanes(original.ID, secondAdd.Pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{original.ID: true, firstAdd.Pane.ID: true, secondAdd.Pane.ID: true}
	if !samePaneSet(desiredPaneIDs(workspace), expected) {
		t.Fatalf("concurrent add lost a pane: %#v", desiredPaneIDs(workspace))
	}
	if workspace.Topology.Generation != baseGeneration+2 {
		t.Fatalf("topology generation = %d, want %d", workspace.Topology.Generation, baseGeneration+2)
	}
	if workspace.Attachments[secondAttachment.ID].ReconcileStatus != TopologyReconcilePending {
		t.Fatalf("stale source falsely claimed convergence: %#v", workspace.Attachments[secondAttachment.ID])
	}
}

func TestConcurrentMetadataEditsMergeByStableNodeID(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	firstAttachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "first", Node: Host{ID: "first"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:first",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: firstAttachment.ID,
		Manifest: manifestForPanes(workspace, pane.ID), Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	baseGeneration := workspace.Topology.Generation
	secondAttachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "second", Node: Host{ID: "second"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:second",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: secondAttachment.ID,
		TopologyGeneration: baseGeneration, TopologyDigest: workspace.Topology.Digest,
		ObservedTopology: workspace.Topology.Roots,
		Status:           AttachmentReady, Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}

	renamed := manifestForPanes(workspace, pane.ID)
	renamed.Topology[0].Children[0].Title = "Renamed elsewhere"
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: firstAttachment.ID,
		BaseTopologyGeneration: baseGeneration, Manifest: renamed, Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	relayout := manifestForPanes(workspace, pane.ID)
	relayout.Topology[0].Children[0].Layout = "grid"
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: secondAttachment.ID,
		BaseTopologyGeneration: baseGeneration, Manifest: relayout, Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	tab := workspace.Topology.Roots[0].Children[0]
	if tab.Title != "Renamed elsewhere" || tab.Layout != "grid" {
		t.Fatalf("concurrent metadata did not merge: %#v", tab)
	}
}

func TestStaleCaptureCannotOmitPaneFromVerifiedBaseline(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	first, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "first", Node: Host{ID: "first"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:first",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: first.ID,
		Manifest: manifestForPanes(workspace, panes[0].ID, panes[1].ID),
		Views:    viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	baseGeneration := workspace.Topology.Generation
	second, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "second", Node: Host{ID: "second"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:second",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: second.ID,
		TopologyGeneration: baseGeneration, TopologyDigest: workspace.Topology.Digest,
		ObservedTopology: workspace.Topology.Roots,
		Status:           AttachmentReady, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	renamed := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	renamed.Topology[0].Children[0].Title = "newer"
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: first.ID,
		BaseTopologyGeneration: baseGeneration, Manifest: renamed,
		Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: second.ID,
		BaseTopologyGeneration: baseGeneration,
		Manifest:               manifestForPanes(workspace, panes[0].ID),
		Views:                  viewsForPanes(panes[0].ID),
	})
	if err == nil || !strings.Contains(err.Error(), "omitted previously observed pane") {
		t.Fatalf("stale omission error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if !samePaneSet(desiredPaneIDs(current), map[string]bool{panes[0].ID: true, panes[1].ID: true}) {
		t.Fatalf("stale omission changed desired panes: %#v", desiredPaneIDs(current))
	}
}

func TestReconcileVerificationIsFencedByTopologyGeneration(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "source", Node: Host{ID: "source"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:source",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifestForPanes(workspace, pane.ID), Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := workspace.Topology.Generation
	renamed := manifestForPanes(workspace, pane.ID)
	renamed.Topology[0].Children[0].Title = "newer"
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		BaseTopologyGeneration: oldGeneration, Manifest: renamed, Views: viewsForPanes(pane.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		BaseTopologyGeneration: oldGeneration, VerifyTopologyGeneration: oldGeneration,
		Manifest: manifestForPanes(workspace, pane.ID), Views: viewsForPanes(pane.ID),
	})
	if err == nil || !strings.Contains(err.Error(), "topology generation changed") {
		t.Fatalf("verification fence error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if current.Topology.Roots[0].Children[0].Title != "newer" {
		t.Fatalf("stale reconcile overwrote newer topology: %#v", current.Topology.Roots)
	}
}
