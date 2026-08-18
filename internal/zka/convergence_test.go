package zka

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The convergence digest must cover structure and nothing else. Every field
// listed here can legitimately differ between two Kitty processes showing the
// same workspace -- split biases follow window geometry, enabled_layouts and
// wm_class come from each process's own config, titles are owned by the running
// program. Letting any of them into identity is what made convergence
// impossible to reach.
func TestStructuralDigestIgnoresPresentation(t *testing.T) {
	base := []Node{{ID: "os", Kind: "os-window", Children: []Node{{
		ID: "tab", Kind: "tab", Children: []Node{{ID: "a", Kind: "pane", PaneID: "a"}},
	}}}}
	variants := map[string]func([]Node){
		"tab title":  func(n []Node) { n[0].Children[0].Title = "renamed" },
		"tab layout": func(n []Node) { n[0].Children[0].Layout = "grid" },
		"enabled layouts": func(n []Node) {
			n[0].Children[0].EnabledLayouts = []string{"tall", "splits"}
		},
		"split bias": func(n []Node) {
			n[0].Children[0].LayoutState = json.RawMessage(`{"main_bias":[0.7,0.3]}`)
		},
		"os window class": func(n []Node) { n[0].Class = "other" },
		"os window name":  func(n []Node) { n[0].Name = "other" },
		"os window state": func(n []Node) { n[0].State = "fullscreen" },
		"pane title":      func(n []Node) { n[0].Children[0].Children[0].Title = "vim" },
		"pane cwd":        func(n []Node) { n[0].Children[0].Children[0].CWD = "/elsewhere" },
		"focus":           func(n []Node) { n[0].Children[0].Children[0].Focused = true },
	}
	want := topologyStructuralDigest(base)
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			variant := cloneNodes(base)
			mutate(variant)
			if got := topologyStructuralDigest(variant); got != want {
				t.Fatalf("%s changed structural identity; it would force every other attachment to rebuild", name)
			}
		})
	}
}

// The converse: anything that really is structure must move the digest.
func TestStructuralDigestTracksStructure(t *testing.T) {
	base := []Node{{ID: "os", Kind: "os-window", Children: []Node{
		{ID: "t1", Kind: "tab", Children: []Node{{ID: "a", Kind: "pane", PaneID: "a"}}},
		{ID: "t2", Kind: "tab", Children: []Node{{ID: "b", Kind: "pane", PaneID: "b"}}},
	}}}
	want := topologyStructuralDigest(base)
	variants := map[string][]Node{
		"panes merged into one tab": {{ID: "os", Kind: "os-window", Children: []Node{
			{ID: "t1", Kind: "tab", Children: []Node{
				{ID: "a", Kind: "pane", PaneID: "a"}, {ID: "b", Kind: "pane", PaneID: "b"},
			}},
		}}},
		"tab order swapped": {{ID: "os", Kind: "os-window", Children: []Node{
			{ID: "t2", Kind: "tab", Children: []Node{{ID: "b", Kind: "pane", PaneID: "b"}}},
			{ID: "t1", Kind: "tab", Children: []Node{{ID: "a", Kind: "pane", PaneID: "a"}}},
		}}},
		"pane removed": {{ID: "os", Kind: "os-window", Children: []Node{
			{ID: "t1", Kind: "tab", Children: []Node{{ID: "a", Kind: "pane", PaneID: "a"}}},
		}}},
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			if topologyStructuralDigest(variant) == want {
				t.Fatalf("%s did not change structural identity", name)
			}
		})
	}
}

// Kitty and the compositor do not expose a commandable ordering for
// independent OS windows, so their ls order must not create a new target.
func TestStructuralDigestIgnoresOSWindowOrder(t *testing.T) {
	left := []Node{
		{ID: "os-a", Kind: "os-window", Children: []Node{{
			ID: "tab-a", Kind: "tab", Children: []Node{{ID: "a", Kind: "pane", PaneID: "a"}},
		}}},
		{ID: "os-b", Kind: "os-window", Children: []Node{{
			ID: "tab-b", Kind: "tab", Children: []Node{{ID: "b", Kind: "pane", PaneID: "b"}},
		}}},
	}
	right := []Node{left[1], left[0]}
	if topologyStructuralDigest(left) != topologyStructuralDigest(right) {
		t.Fatal("uncommandable OS-window order changed structural identity")
	}
	workspace := &Workspace{
		ID: "workspace", Panes: map[string]*Pane{
			"a": {ID: "a", Phase: PaneAdmitted},
			"b": {ID: "b", Phase: PaneAdmitted},
		},
	}
	if _, err := installDesiredTopology(workspace, left, topologyInstallSystem); err != nil {
		t.Fatal(err)
	}
	stored := cloneNodes(workspace.Topology.Roots)
	if changed, err := installDesiredTopology(workspace, right, topologyInstallVerify); err != nil || changed {
		t.Fatalf("reordered OS roots changed desired topology: changed=%v err=%v", changed, err)
	}
	if !nodesEqual(stored, workspace.Topology.Roots) {
		t.Fatalf("OS-root observation order churned stored topology: %#v", workspace.Topology.Roots)
	}
}

// Dragging a split divider must not ripple out to other attachments. Under the
// old digest it bumped the generation, which demoted every peer and triggered a
// full rebuild in each of them.
func TestSplitGeometryChangeDoesNotAdvanceGeneration(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/kitty",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	manifest.Topology[0].Children[0].LayoutState = json.RawMessage(`{"main_bias":[0.5,0.5]}`)
	workspace, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifest, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, revision := workspace.Topology.Generation, workspace.Revision

	dragged := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	dragged.Topology[0].Children[0].LayoutState = json.RawMessage(`{"main_bias":[0.71,0.29]}`)
	after, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: revision,
		Manifest: dragged, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Topology.Generation != generation {
		t.Fatalf("a divider drag advanced the generation: %d -> %d", generation, after.Topology.Generation)
	}
	if after.Attachments[attachment.ID].Status != AttachmentReady {
		t.Fatalf("a divider drag knocked the attachment out of ready: %#v", after.Attachments[attachment.ID])
	}
	// The new geometry is still stored, so a cold restore reproduces it.
	if !strings.Contains(string(after.Topology.Roots[0].Children[0].LayoutState), "0.71") {
		t.Fatalf("layout state was not replicated: %s", after.Topology.Roots[0].Children[0].LayoutState)
	}
}

// A failed enforcement pass may leave Kitty partially rearranged, but that
// local state is never allowed to replace the desired topology. Persistent
// failure stalls layout reconciliation for this generation instead.
func TestUnconvergedAttachmentPreservesDesiredTopology(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	generation := workspace.Topology.Generation

	// Kitty reports the two panes in separate tabs; the desired topology has
	// them in one, and this fake never moves.
	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[0].ID}, {panes[1].ID}})
	current, _ := d.getWorkspace(workspace.ID)
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, current)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	for attempt := 1; attempt <= maxEnforceAttempts; attempt++ {
		d.runTopologyReconcile(context.Background(), attachment.Endpoint)
		got, getErr := d.getWorkspace(workspace.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Topology.Generation != generation || got.Topology.Digest != workspace.Topology.Digest {
			t.Fatalf("failed pass %d published observed topology: %#v", attempt, got.Topology)
		}
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	stalled := got.Attachments[attachment.ID]
	if stalled.Status != AttachmentLayoutStalled || stalled.ReconcileStatus != TopologyReconcileError {
		t.Fatalf("persistent failure did not stall layout reconciliation: %#v", stalled)
	}
	if d.endpointNeedsTopologyReconcile(attachment.Endpoint) {
		t.Fatal("layout-stalled generation was automatically re-armed")
	}
	// A delayed watcher capture of the same partial tree is also non-authoritative.
	d.captureEndpoint(context.Background(), attachment.Endpoint)
	afterCapture, _ := d.getWorkspace(workspace.ID)
	if afterCapture.Topology.Generation != generation || afterCapture.Topology.Digest != workspace.Topology.Digest {
		t.Fatalf("watcher published partial topology after stall: %#v", afterCapture.Topology)
	}
	if afterCapture.Attachments[attachment.ID].Status != AttachmentLayoutStalled ||
		afterCapture.Attachments[attachment.ID].ReconcileStatus != TopologyReconcileError {
		t.Fatalf("watcher capture re-armed a stalled generation: %#v", afterCapture.Attachments[attachment.ID])
	}
}

func TestOperatorAdoptionRequiresPreviewAndStableCandidate(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	generation := workspace.Topology.Generation
	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[0].ID}, {panes[1].ID}})
	current, _ := d.getWorkspace(workspace.ID)
	d.kitty.Runner = &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, current)
	}}
	d.kitty.Command = "kitten-test"

	preview, err := d.adoptLayout(context.Background(), topologyAdoptRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.ConfirmToken == "" || preview.CandidateDigest == workspace.Topology.Digest {
		t.Fatalf("invalid adoption preview: %#v", preview)
	}
	unchanged, _ := d.getWorkspace(workspace.ID)
	if unchanged.Topology.Generation != generation {
		t.Fatal("adoption preview mutated the desired topology")
	}
	applied, err := d.adoptLayout(context.Background(), topologyAdoptRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Confirm: preview.ConfirmToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Workspace.Topology.Generation != generation+1 ||
		applied.Workspace.Topology.Digest != preview.CandidateDigest {
		t.Fatalf("confirmed adoption was not applied: %#v", applied)
	}
}

// Operator adoption is still bounded by the pane set: a capture that has lost
// a live pane must never become the desired state.
func TestOperatorAdoptionRefusesToChangeThePaneSet(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()

	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[0].ID}})
	current, _ := d.getWorkspace(workspace.ID)
	d.kitty.Runner = &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, current)
	}}
	d.kitty.Command = "kitten-test"

	if _, err := d.adoptLayout(context.Background(), topologyAdoptRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
	}); err == nil {
		t.Fatal("adoption accepted a capture that dropped a live pane")
	}
	got, _ := d.getWorkspace(workspace.ID)
	if !samePaneSet(desiredPaneIDs(got), map[string]bool{panes[0].ID: true, panes[1].ID: true}) {
		t.Fatalf("refused adoption still changed the pane set: %#v", desiredPaneIDs(got))
	}
}

// A permanently errored attachment must stop being re-armed by the fallback
// timer. Treating "not converged yet" as terminal, and terminal as retryable,
// is what turned a cosmetic mismatch into an endless rebuild loop.
func TestFatalReconcileStopsFallbackRearming(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")

	if d.endpointNeedsTopologyReconcile(attachment.Endpoint) {
		t.Fatal("a converged attachment should not need reconciliation")
	}
	d.markAttachmentReconcileError(workspace.ID, attachment.ID, errTopologyInvalid)
	if d.endpointNeedsTopologyReconcile(attachment.Endpoint) {
		t.Fatal("a terminally errored attachment must not be re-armed on a timer")
	}
	d.markAttachmentReconcilePending(workspace.ID, attachment.ID)
	if !d.endpointNeedsTopologyReconcile(attachment.Endpoint) {
		t.Fatal("a pending attachment must be retried")
	}
}

// The applying marker is persisted before Kitty is mutated. If the daemon
// exits mid-pass, the next process must resume that generation rather than
// treating the stale applied digest as proof of convergence.
func TestApplyingAtCurrentGenerationResumesAfterRestart(t *testing.T) {
	root := testRoot(t)
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")

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
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.endpointNeedsTopologyReconcile(attachment.Endpoint) {
		t.Fatal("persisted applying state was not resumed after restart")
	}
}

// Unrecognised errors must be retried, not treated as permanent damage. The old
// substring matcher defaulted the other way and swallowed every context
// deadline and every raw kitten failure into a terminal state.
func TestUnknownReconcileErrorIsRetriedNotFatal(t *testing.T) {
	cases := map[string]struct {
		err  error
		want reconcileClass
	}{
		"unknown":            {errors.New("something entirely new"), reconcileRetryBackoff},
		"kitty timeout":      {context.DeadlineExceeded, reconcileRetryBackoff},
		"kitten failure":     {errKittyCommand, reconcileRetryBackoff},
		"missing ready pane": {errViewsNotReady, reconcileRetryBackoff},
		"revision changed":   {errWorkspaceRevisionChanged, reconcileRetryBackoff},
		"generation changed": {errTopologyGenerationChanged, reconcileRetryFast},
		"admission pending":  {errPaneAdmissionPending, reconcileRetryFast},
		"invalid topology":   {errTopologyInvalid, reconcileFatal},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classifyReconcileError(testCase.err, 1, maxEnforceAttempts); got != testCase.want {
				t.Fatalf("class = %d, want %d", got, testCase.want)
			}
		})
	}
}

// A failed install must leave the workspace byte-identical, and above all still
// renderable: promoting a pane while leaving it out of Roots made the workspace
// permanently unattachable.
func TestFailedTopologyInstallLeavesWorkspaceRenderable(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()
	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := d.getWorkspace(workspace.ID)

	// A capture that includes the proposed pane but drops an admitted one.
	_, err = d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		Manifest: manifestForPanes(before, panes[0].ID, allocated.Pane.ID),
		Views:    viewsForPanes(panes[0].ID, allocated.Pane.ID),
	})
	if err == nil {
		t.Fatal("a capture omitting an admitted pane was accepted")
	}
	after, _ := d.getWorkspace(workspace.ID)
	if !after.Panes[allocated.Pane.ID].Proposed() {
		t.Fatal("failed install promoted a pane that never made it into the topology")
	}
	if after.Topology.Digest != before.Topology.Digest || after.Topology.Generation != before.Topology.Generation {
		t.Fatalf("failed install mutated the desired topology: %#v", after.Topology)
	}
	if _, err := renderDesiredTopologySession(after, Transport{Kind: "local"}, ""); err != nil {
		t.Fatalf("workspace is no longer renderable after a failed install: %v", err)
	}
}

// The stored digest must always describe the stored tree. Rewriting Roots after
// the digest was computed could persist a target unreachable by construction.
func TestStoredDigestAlwaysDescribesStoredRoots(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()

	renamed := manifestForPanes(workspace, panes[0].ID, panes[1].ID)
	renamed.Topology[0].Children[0].Children[0].Title = "vim"
	if _, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Manifest: renamed, Views: viewsForPanes(panes[0].ID, panes[1].ID),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.getWorkspace(workspace.ID)
	if digest := topologyStructuralDigest(got.Topology.Roots); digest != got.Topology.Digest {
		t.Fatalf("stored digest %s does not describe stored roots (%s)", got.Topology.Digest, digest)
	}
}
