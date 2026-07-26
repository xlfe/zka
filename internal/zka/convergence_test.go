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

// Dragging a split divider must not ripple out to other attachments. Under the
// old digest it bumped the generation, which demoted every peer and triggered a
// full rebuild in each of them.
func TestSplitGeometryChangeDoesNotAdvanceGeneration(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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

// The liveness guarantee. Whatever the reason enforcement keeps failing, the
// daemon must eventually accept what Kitty actually shows instead of fighting
// it forever. Without this, one unreproducible node destroyed the session on a
// timer indefinitely.
func TestUnconvergedAttachmentAdoptsObservedTopology(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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

	err = d.reconcileEndpointTopology(context.Background(), attachment.Endpoint)
	if !errors.Is(err, errStructureNotConverged) {
		t.Fatalf("reconcile error = %v, want errStructureNotConverged", err)
	}
	if classifyReconcileError(err, 1, maxEnforceAttempts) != reconcileRetryBackoff {
		t.Fatal("first non-convergence should retry with backoff")
	}
	if classifyReconcileError(err, maxEnforceAttempts, maxEnforceAttempts) != reconcileAdopt {
		t.Fatal("persistent non-convergence must escalate to adoption, not to a permanent error")
	}

	if err := d.adoptObservedTopology(context.Background(), attachment.Endpoint); err != nil {
		t.Fatalf("adopt observed topology: %v", err)
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Topology.Generation <= generation {
		t.Fatalf("adoption did not advance the generation: %d", got.Topology.Generation)
	}
	if len(got.Topology.Roots[0].Children) != 2 {
		t.Fatalf("adoption did not take the observed grouping: %#v", got.Topology.Roots)
	}
	// The pane set is the one thing adoption may never change.
	if !samePaneSet(topologyPaneIDs(got.Topology.Roots), map[string]bool{panes[0].ID: true, panes[1].ID: true}) {
		t.Fatalf("adoption changed the pane set: %#v", topologyPaneIDs(got.Topology.Roots))
	}
	// Having adopted, the very next pass converges.
	if err := d.reconcileEndpointTopology(context.Background(), attachment.Endpoint); err != nil {
		t.Fatalf("reconcile after adoption still failed: %v", err)
	}
}

// Adoption is bounded by the pane set: a capture that has lost a live pane must
// never become the desired state.
func TestAdoptionRefusesToChangeThePaneSet(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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

	if err := d.adoptObservedTopology(context.Background(), attachment.Endpoint); err == nil {
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
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
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
