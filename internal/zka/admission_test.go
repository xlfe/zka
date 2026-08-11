package zka

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The outage in one test. Opening a Kitty tab allocates a pane whose zmx
// backend comes up within milliseconds -- long before any capture can land. The
// backend reconciler used to see "pending pane, live backend" and fabricate a
// synthetic topology tab for it, with no enabled_layouts and no layout_state.
// Kitty always reports both, so the desired topology instantly became something
// Kitty could never reproduce, and every window was destroyed and recreated
// every ~30 seconds from then on.
func TestBackendReconcileNeverFabricatesTopologyForProposedPane(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	baseGeneration := workspace.Topology.Generation
	baseDigest := workspace.Topology.Digest

	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	// The zmx backend is alive but no Kitty capture has admitted the pane yet.
	runner.setSession(allocated.Pane.Backend.Ref, true)

	for i := 0; i < 3; i++ {
		response, err := d.reconcileBackends(context.Background(), workspace.ID)
		if err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
		if len(response.Recovered) != 0 {
			t.Fatalf("pass %d fabricated topology for %v", i, response.Recovered)
		}
	}

	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Topology.Generation != baseGeneration || got.Topology.Digest != baseDigest {
		t.Fatalf("proposed pane moved the desired topology: generation %d -> %d",
			baseGeneration, got.Topology.Generation)
	}
	if topologyPaneIDs(got.Topology.Roots)[allocated.Pane.ID] {
		t.Fatal("proposed pane was inserted into the desired topology without Kitty evidence")
	}
	if pane := got.Panes[allocated.Pane.ID]; pane == nil || !pane.Proposed() {
		t.Fatalf("proposed pane = %#v, want a surviving proposed pane", pane)
	}
	assertNoFabricatedTabs(t, got)
	if status := got.Attachments[attachment.ID].ReconcileStatus; status == TopologyReconcileError {
		t.Fatalf("attachment was invalidated by a proposed pane: %s", status)
	}
}

// The normal path: once Kitty reports the tagged window, admission commits the
// pane from that real capture, and the resulting node carries the metadata a
// capture supplies rather than invented blanks.
func TestPaneAdmissionCommitsFromRealCapture(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	first := firstPane(workspace)
	baseGeneration := workspace.Topology.Generation

	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	admitEndpoint(t, d, workspace.ID, allocated.Pane.ID, attachment.Endpoint)

	tree := kittyTreeForTabs(workspace.ID, [][]string{{first.ID, allocated.Pane.ID}})
	current, _ := d.getWorkspace(workspace.ID)
	kittyRunner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, current)
	}}
	d.kitty.Runner = kittyRunner
	d.kitty.Command = "kitten-test"

	if err := d.admitPendingPanes(context.Background(), attachment.Endpoint); err != nil {
		t.Fatalf("admit pending panes: %v", err)
	}

	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	pane := got.Panes[allocated.Pane.ID]
	if pane == nil || !pane.Admitted() {
		t.Fatalf("pane phase = %#v, want admitted", pane)
	}
	if !pane.Admission.MissingSince.IsZero() {
		t.Fatal("admitted pane kept absence evidence")
	}
	if got.Topology.Generation <= baseGeneration {
		t.Fatalf("admission did not advance the generation: %d", got.Topology.Generation)
	}
	if !samePaneSet(topologyPaneIDs(got.Topology.Roots), map[string]bool{first.ID: true, allocated.Pane.ID: true}) {
		t.Fatalf("admitted topology panes = %#v", topologyPaneIDs(got.Topology.Roots))
	}
	assertNoFabricatedTabs(t, got)

	// The committed node must carry what a capture reports, and re-observing
	// the same tree must already agree -- no rebuild required.
	tab := got.Topology.Roots[0].Children[0]
	if len(tab.EnabledLayouts) == 0 || len(tab.LayoutState) == 0 {
		t.Fatalf("admitted tab lost capture metadata: %#v", tab)
	}
	observed, err := topologyFromKitty(tree, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed, err = stabilizeTopologyIDs(got.ID, got.Topology.Roots, observed); err != nil {
		t.Fatal(err)
	}
	if !topologyMatchesDesired(got, observed) {
		t.Fatal("topology committed by admission does not match the tree it came from")
	}
	for _, call := range kittyRunner.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "goto_session") {
			t.Fatalf("admission rebuilt the session: %#v", call.Args)
		}
	}
}

// A proposed pane whose window is present is never retired, however old it is.
// Retiring it would leave Kitty showing a window tagged for an unknown pane,
// which wedges every later capture.
func TestProposedPaneWithLiveWindowIsNeverRetired(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	admitEndpoint(t, d, workspace.ID, allocated.Pane.ID, attachment.Endpoint)
	runner.setSession(allocated.Pane.Backend.Ref, true)

	// Age the pane far past every deadline.
	d.mu.Lock()
	pane := d.state.Workspaces[workspace.ID].Panes[allocated.Pane.ID]
	pane.PhaseAt = time.Now().UTC().Add(-10 * paneAdmissionDeadline)
	pane.UpdatedAt = pane.PhaseAt
	d.mu.Unlock()

	if _, err := d.reconcileBackends(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := d.getWorkspace(workspace.ID)
	if pane := got.Panes[allocated.Pane.ID]; pane == nil || !pane.Proposed() {
		t.Fatalf("pane with a live Kitty window was retired: %#v", pane)
	}
	for _, call := range runner.Calls() {
		if len(call.Args) > 1 && call.Args[0] == "kill" {
			t.Fatalf("backend of a live proposed pane was killed: %#v", call.Args)
		}
	}
}

// A proposed pane whose window is absent from successful listings is retired
// through the normal closure path, so its zmx session is cleaned up rather than
// leaked.
func TestProposedPaneMissingFromKittyIsRetired(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	admitEndpoint(t, d, workspace.ID, allocated.Pane.ID, attachment.Endpoint)
	runner.setSession(allocated.Pane.Backend.Ref, true)
	runner.mu.Lock()
	runner.failKill = true
	runner.mu.Unlock()

	d.mu.Lock()
	pane := d.state.Workspaces[workspace.ID].Panes[allocated.Pane.ID]
	// A successful listing already reported the window as absent, long ago.
	pane.Admission.MissingSince = time.Now().UTC().Add(-2 * paneAdmissionGrace)
	pane.UpdatedAt = pane.Admission.MissingSince
	d.mu.Unlock()

	if _, err := d.reconcileBackends(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, call := range runner.Calls() {
			if len(call.Args) >= 2 && call.Args[0] == "kill" && call.Args[1] == allocated.Pane.Backend.Ref {
				return true
			}
		}
		return false
	})
	got, _ := d.getWorkspace(workspace.ID)
	if pane := got.Panes[allocated.Pane.ID]; pane == nil || !pane.Retiring() {
		t.Fatalf("pane phase = %#v, want retiring", pane)
	}

	runner.mu.Lock()
	runner.failKill = false
	runner.mu.Unlock()
	waitFor(t, func() bool {
		got, getErr := d.getWorkspace(workspace.ID)
		return getErr == nil && got.Panes[allocated.Pane.ID] == nil
	})
}

// Absence evidence may only come from a successful listing. A Kitty remote
// control outage must never be able to destroy a pane.
func TestAdmissionRecordsAbsenceOnlyFromSuccessfulListings(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	allocated, err := testAllocatePane(d, workspace.ID, "", "/work")
	if err != nil {
		t.Fatal(err)
	}
	admitEndpoint(t, d, workspace.ID, allocated.Pane.ID, attachment.Endpoint)

	d.kitty.Runner = &fakeRunner{handler: func(_ context.Context, _ string, _ ...string) (string, string, error) {
		return "", "kitty is not responding", context.DeadlineExceeded
	}}
	d.kitty.Command = "kitten-test"
	for i := 0; i < 5; i++ {
		_ = d.admitPendingPanes(context.Background(), attachment.Endpoint)
	}
	got, _ := d.getWorkspace(workspace.ID)
	if !got.Panes[allocated.Pane.ID].Admission.MissingSince.IsZero() {
		t.Fatal("a failed Kitty listing produced absence evidence")
	}
}

// An idle endpoint must cost nothing: no proposed pane means no Kitty command
// at all. This is the contract the idle-update-storm fix established.
func TestAdmissionIsSilentWithoutProposedPanes(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	runner := &fakeRunner{}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	before, _ := d.getWorkspace(workspace.ID)
	d.schedulePaneAdmission(attachment.Endpoint)
	if err := d.admitPendingPanes(context.Background(), attachment.Endpoint); err != nil {
		t.Fatal(err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("idle admission issued %d Kitty commands: %#v", len(calls), calls)
	}
	after, _ := d.getWorkspace(workspace.ID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("idle admission advanced workspace timestamp: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// Retried allocation for the same Kitty window must be idempotent, but a
// restarted Kitty reusing window id 1 must not collide with an admitted pane.
func TestLocalPaneAllocationIsIdempotentPerKittyWindow(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	request := workspacePaneRequest{Workspace: workspace.ID, Endpoint: "unix:/kitty", WindowID: 12, CWD: "/work"}
	first, err := d.preparePane(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.preparePane(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pane.ID != second.Pane.ID {
		t.Fatalf("retried prepare allocated a second pane: %s vs %s", first.Pane.ID, second.Pane.ID)
	}

	d.mu.Lock()
	d.state.Workspaces[workspace.ID].Panes[first.Pane.ID].Phase = PaneAdmitted
	d.mu.Unlock()
	third, err := d.preparePane(request)
	if err != nil {
		t.Fatal(err)
	}
	if third.Pane.ID == first.Pane.ID {
		t.Fatal("a reused Kitty window id was matched against an already admitted pane")
	}
}

func admitEndpoint(t *testing.T, d *Daemon, workspaceID, paneID, endpoint string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	pane := d.state.Workspaces[workspaceID].Panes[paneID]
	pane.Admission.Endpoint = endpoint
}

func assertNoFabricatedTabs(t *testing.T, workspace *Workspace) {
	t.Helper()
	var visit func([]Node)
	visit = func(nodes []Node) {
		for _, node := range nodes {
			if node.Kind == "tab" && strings.HasPrefix(node.Title, "Recovered ") {
				t.Fatalf("desired topology contains a fabricated tab: %#v", node)
			}
			visit(node.Children)
		}
	}
	visit(workspace.Topology.Roots)
}
