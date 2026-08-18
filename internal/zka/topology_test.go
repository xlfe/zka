package zka

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A pane missing from the persisted topology must come back as *proposed*, to
// be admitted by the next real Kitty capture. The previous behaviour -- which
// this test used to assert -- fabricated a synthetic "Recovered" tab carrying
// no enabled_layouts and no layout_state. Kitty always reports both, so that
// tab could never match and the workspace rebuilt itself every 30 seconds
// forever. Fabricating topology is now banned outright.
func TestStateLoadProposesLivePaneMissingFromManifestWithoutFabricatingTopology(t *testing.T) {
	paths := testPaths(testRoot(t))
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
						ID: "visible", Position: 0, Phase: PaneAdmitted,
						Backend: BackendRef{Kind: "zmx", Ref: "visible"}, CreatedAt: now, UpdatedAt: now,
					},
					"orphan": {
						ID: "orphan", Position: 1, Phase: PaneProposed, BackendCreated: true, BackendReady: true,
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
	if got := topologyPaneIDs(workspace.Topology.Roots); !samePaneSet(got, map[string]bool{"visible": true}) {
		t.Fatalf("migrated panes = %#v, want only the pane the manifest described", got)
	}
	if !workspace.Panes["orphan"].Proposed() {
		t.Fatalf("orphan phase = %q, want proposed so a real capture can admit it", workspace.Panes["orphan"].Phase)
	}
	if len(workspace.Topology.Roots[0].Children[0].LayoutState) != 0 {
		t.Fatalf("migration retained attachment-local layout ids: %s", workspace.Topology.Roots[0].Children[0].LayoutState)
	}
	for _, tab := range workspace.Topology.Roots[0].Children {
		if strings.HasPrefix(tab.Title, "Recovered ") {
			t.Fatalf("state load fabricated a synthetic tab Kitty cannot reproduce: %#v", tab)
		}
	}
	if digest := topologyStructuralDigest(workspace.Topology.Roots); digest != workspace.Topology.Digest {
		t.Fatalf("stored digest %s does not describe stored roots (%s)", workspace.Topology.Digest, digest)
	}
	attachment := workspace.Attachments["local"]
	if attachment.AppliedTopologyGeneration != 0 || attachment.ReconcileStatus != "pending" {
		t.Fatalf("migration trusted stale attachment: %#v", attachment)
	}
	assertMigrationBackup(t, paths.StateFile, 4, encoded)
}

func TestPartialCaptureCannotHideActivePane(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	if got.Topology.Generation != 0 || !got.Panes[panes[0].ID].Admitted() || !got.Panes[panes[1].ID].Admitted() {
		t.Fatalf("partial capture mutated canonical pane membership: %#v", got)
	}
}

func TestMirrorCaptureCommitsPendingPaneToCanonicalTopology(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	allocated, err := testAllocatePane(d, workspace.ID, "mirror:add", "/remote/work")
	if err != nil {
		t.Fatal(err)
	}
	second := allocated.Pane
	if !second.Proposed() {
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
	if workspace.Panes[second.ID].Proposed() {
		t.Fatal("committed pane remained topology-pending")
	}
	if workspace.Attachments[source.ID].ReconcileStatus != "pending" {
		t.Fatalf("other attachment was not invalidated: %#v", workspace.Attachments[source.ID])
	}
}

func TestReadyAttachmentRequiresVerifiedTopologyDigest(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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

func TestStabilizeTopologyIDsRejectsAttachmentLocalContainerTags(t *testing.T) {
	previous := []Node{{
		ID: "canonical-os", Kind: "os-window", Children: []Node{{
			ID: "canonical-tab", Kind: "tab", Children: []Node{
				{ID: "a", Kind: "pane", PaneID: "a"},
				{ID: "b", Kind: "pane", PaneID: "b"},
			},
		}},
	}}
	captured := cloneNodes(previous)
	captured[0].ID = "attachment-os"
	captured[0].Children[0].ID = "attachment-tab"

	stable, err := stabilizeTopologyIDs("workspace", previous, captured)
	if err != nil {
		t.Fatal(err)
	}
	if stable[0].ID != "canonical-os" || stable[0].Children[0].ID != "canonical-tab" {
		t.Fatalf("attachment-local container tags displaced canonical identity: %#v", stable)
	}
}

func TestReadyUserCaptureMayPublishStructuralChange(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	baseGeneration := workspace.Topology.Generation
	manifest := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	manifest.Topology[0].Children = []Node{
		{Kind: "tab", Layout: "splits", Children: []Node{{Kind: "pane", PaneID: panes[0].ID}}},
		{Kind: "tab", Layout: "splits", Children: []Node{{Kind: "pane", PaneID: panes[1].ID}}},
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifest, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateUserCapture)

	updated, err := d.updateManifest(request)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Topology.Generation != baseGeneration+1 || len(updated.Topology.Roots[0].Children) != 2 {
		t.Fatalf("ready user capture did not publish its structural edit: %#v", updated.Topology)
	}
}

func TestApplyingSourceDeclarationCannotPublishStructuralChange(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	manifest := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	manifest.Topology[0].Children = []Node{
		{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: panes[0].ID}}},
		{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: panes[1].ID}}},
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifest, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateUserCapture)
	request.SourceStatus = AttachmentPreparing
	request.SourceReconcileStatus = TopologyReconcileApplying

	if _, err := d.updateManifest(request); !errors.Is(err, errStructuralPublicationRefused) {
		t.Fatalf("applying source publication error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if current.Topology.Generation != workspace.Topology.Generation || current.Topology.Digest != workspace.Topology.Digest {
		t.Fatal("refused applying source changed the desired topology")
	}
}

func TestAdmissionRefusesHalfAppliedExistingPaneProjection(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, _ = readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	allocated, err := testAllocatePane(d, workspace.ID, "new", "")
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ = d.getWorkspace(workspace.ID)
	attachment := workspace.Attachments["local"]
	manifest := manifestForPanes(workspace, panes[0].ID, panes[1].ID, allocated.Pane.ID)
	manifest.Topology[0].Children = []Node{
		{Kind: "tab", Children: []Node{
			{Kind: "pane", PaneID: panes[1].ID},
			{Kind: "pane", PaneID: panes[0].ID},
		}},
		{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: allocated.Pane.ID}}},
	}
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifest, Views: viewsForPanes(panes[0].ID, panes[1].ID, allocated.Pane.ID),
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateAdmission)

	if _, err := d.updateManifest(request); !errors.Is(err, errStructuralPublicationRefused) {
		t.Fatalf("half-applied admission error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if !current.Panes[allocated.Pane.ID].Proposed() ||
		current.Topology.Generation != workspace.Topology.Generation ||
		current.Topology.Digest != workspace.Topology.Digest {
		t.Fatalf("refused admission mutated canonical state: %#v", current)
	}
}

func TestPresentationVerificationMayCompleteFromApplyingState(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	pane := firstPane(workspace)

	d.mu.Lock()
	stored := d.state.Workspaces[workspace.ID].Attachments[attachment.ID]
	stored.Status = AttachmentPreparing
	stored.ReconcileStatus = TopologyReconcileApplying
	stored.ReconcileTargetGeneration = workspace.Topology.Generation
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	workspace, _ = d.getWorkspace(workspace.ID)
	attachment = workspace.Attachments[attachment.ID]
	manifest := manifestForPanes(workspace, pane.ID)
	manifest.Topology[0].Children[0].Title = "renamed"
	request := manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		VerifyTopologyGeneration: workspace.Topology.Generation,
		Manifest:                 manifest, Views: viewsForPanes(pane.ID),
	}
	populateManifestSource(&request, workspace, attachment, topologyUpdateVerify)

	updated, err := d.updateManifest(request)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Topology.Generation != workspace.Topology.Generation ||
		updated.Attachments[attachment.ID].ReconcileStatus != TopologyReconcileReady {
		t.Fatalf("presentation-only verification did not converge applying state: %#v", updated.Attachments[attachment.ID])
	}
}

func TestStoreRefusesSilentTopologyDigestRepair(t *testing.T) {
	root := testRoot(t)
	store := NewStore(testPaths(root))
	workspace := &Workspace{
		ID: "workspace", Panes: map[string]*Pane{
			"pane": {ID: "pane", Phase: PaneAdmitted},
		},
		Topology: DesiredTopology{
			Generation: 1,
			Roots: []Node{{ID: "os", Kind: "os-window", Children: []Node{{
				ID: "tab", Kind: "tab", Children: []Node{{ID: "pane", Kind: "pane", PaneID: "pane"}},
			}}}},
			Digest: "not-the-tree-digest",
		},
	}
	state := newStateData()
	state.Workspaces[workspace.ID] = workspace
	if err := store.Save(state); err == nil || !strings.Contains(err.Error(), "digest disagrees") {
		t.Fatalf("store digest invariant error = %v", err)
	}
	if workspace.Topology.Digest != "not-the-tree-digest" {
		t.Fatal("store silently repaired the caller's inconsistent topology")
	}
}

func TestConcurrentMirrorAddsAreRebasedWithoutDroppingEitherPane(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	firstAdd, err := testAllocatePane(d, workspace.ID, "first:add", "")
	if err != nil {
		t.Fatal(err)
	}
	secondAdd, err := testAllocatePane(d, workspace.ID, "second:add", "")
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

func TestConcurrentPresentationEditsDoNotDisturbStructure(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	// Presentation is deliberately last-writer-wins now: it is replicated on
	// restore and pushed when it differs, but it is not part of structural
	// identity, so it cannot bump the generation or invalidate a peer. What
	// must hold is that a concurrent presentation edit never disturbs
	// structure.
	tab := workspace.Topology.Roots[0].Children[0]
	if tab.Layout != "grid" {
		t.Fatalf("latest presentation edit was lost: %#v", tab)
	}
	if workspace.Topology.Generation != baseGeneration {
		t.Fatalf("presentation edits advanced the generation: %d -> %d",
			baseGeneration, workspace.Topology.Generation)
	}
	if !samePaneSet(topologyPaneIDs(workspace.Topology.Roots), map[string]bool{pane.ID: true}) {
		t.Fatalf("presentation edits disturbed structure: %#v", workspace.Topology.Roots)
	}
}

func TestStaleCaptureCannotOmitPaneFromVerifiedBaseline(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	// Either guard is acceptable; what matters is that a capture omitting a
	// live pane is never installed.
	if err == nil || !(strings.Contains(err.Error(), "omitted previously observed pane") ||
		errors.Is(err, errTopologyPaneSetMismatch)) {
		t.Fatalf("stale omission error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if !samePaneSet(desiredPaneIDs(current), map[string]bool{panes[0].ID: true, panes[1].ID: true}) {
		t.Fatalf("stale omission changed desired panes: %#v", desiredPaneIDs(current))
	}
}

func TestReconcileVerificationIsFencedByTopologyGeneration(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	pane := panes[0]
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "source", Node: Host{ID: "source"}, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:source",
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifestForPanes(workspace, panes[0].ID, panes[1].ID),
		Views:    viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := workspace.Topology.Generation
	// Move the second pane into a tab of its own: a genuine structural change,
	// which is what the generation now tracks.
	regrouped := manifestForPanes(workspace, panes[0].ID)
	regrouped.Topology[0].Children = append(regrouped.Topology[0].Children, Node{
		Kind: "tab", Children: []Node{{Kind: "pane", PaneID: panes[1].ID}},
	})
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		BaseTopologyGeneration: oldGeneration, Manifest: regrouped,
		Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Topology.Generation == oldGeneration {
		t.Fatal("a structural change did not advance the generation")
	}
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		BaseTopologyGeneration: oldGeneration, VerifyTopologyGeneration: oldGeneration,
		Manifest: manifestForPanes(workspace, panes[0].ID, panes[1].ID),
		Views:    viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err == nil || !errors.Is(err, errTopologyGenerationChanged) {
		t.Fatalf("verification fence error = %v", err)
	}
	current, _ := d.getWorkspace(workspace.ID)
	if len(current.Topology.Roots[0].Children) != 2 {
		t.Fatalf("stale reconcile overwrote newer topology: %#v", current.Topology.Roots)
	}
	_ = pane
}
