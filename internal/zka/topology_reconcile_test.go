package zka

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A pane that has been allocated but not yet admitted must never be touched by
// reconciliation. The reconciler yields to admission instead, and reports a
// retryable condition rather than parking the attachment in an error state.
func TestReconcileYieldsToProposedPaneWithoutTouchingIt(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	desired := firstPane(workspace)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	allocated, err := testAllocatePane(d, workspace.ID, "concurrent:add", "")
	if err != nil {
		t.Fatal(err)
	}
	pending := allocated.Pane
	tree := kittyTreeForTabs(workspace.ID, [][]string{{desired.ID, pending.ID}})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, workspace)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	err = d.reconcileEndpointTopology(context.Background(), attachment.Endpoint)
	if !errors.Is(err, errPaneAdmissionPending) {
		t.Fatalf("reconcile error = %v, want errPaneAdmissionPending", err)
	}
	if !topologyReconcileErrorIsTransient(err) {
		t.Fatal("yielding to admission must be transient, not terminal")
	}
	for _, call := range runner.Calls() {
		joined := strings.Join(call.Args, " ")
		for _, destructive := range []string{"close-window", "goto_session", "detach-window", "detach-tab"} {
			if strings.Contains(joined, destructive) {
				t.Fatalf("reconcile ran %q against a workspace holding a proposed pane: %#v", destructive, call.Args)
			}
		}
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Panes[pending.ID].Proposed() {
		t.Fatalf("pending pane phase = %q, want proposed", got.Panes[pending.ID].Phase)
	}
	if got.Attachments[attachment.ID].ReconcileStatus == TopologyReconcileError {
		t.Fatal("yielding to admission must not mark the attachment as errored")
	}
}

// A converged workspace must be completely silent: no Kitty mutation of any
// kind. This is the property that makes the reconciler safe to run on a timer.
func TestConvergedTopologyIssuesNoKittyMutations(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()

	// manifestForPanes puts every pane in one tab, so the live tree must match.
	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[0].ID, panes[1].ID}})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, workspace)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	if err := d.reconcileEndpointTopology(context.Background(), attachment.Endpoint); err != nil {
		t.Fatalf("reconcile of a converged workspace failed: %v", err)
	}
	for _, call := range runner.Calls() {
		joined := strings.Join(call.Args, " ")
		for _, mutation := range []string{
			"close-window", "goto_session", "detach-window", "detach-tab",
			"set-tab-title", "goto-layout", "set-enabled-layouts", "focus-window", "launch",
		} {
			if strings.Contains(joined, mutation) {
				t.Fatalf("converged reconcile issued %q: %#v", mutation, call.Args)
			}
		}
	}
}

// Kitty assigns a new runtime tab id when detach-tab creates an OS window.
// A multi-tab regroup must therefore resolve the destination again after the
// first detach. Reusing the old id makes the second detach fail, leaves the
// workspace in the intermediate one-tab-per-OS-window state, and eventually
// could previously let fallback adoption promote that broken state to every
// attachment.
func TestRegroupTabsResolvesRecreatedDestinationTab(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	if _, err := installDesiredTopology(workspace, []Node{{
		Kind: "os-window",
		Children: []Node{
			{Kind: "tab", Layout: "splits", Children: []Node{{Kind: "pane", PaneID: panes[0].ID}}},
			{Kind: "tab", Layout: "splits", Children: []Node{{Kind: "pane", PaneID: panes[1].ID}}},
		},
	}}, topologyInstallSystem); err != nil {
		t.Fatal(err)
	}

	// The same tabs exist in the same OS window, but in the opposite order.
	// Reconciliation restores the desired order by detaching the desired lead
	// tab into a new OS window, then moving the other tab beside it.
	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[1].ID}, {panes[0].ID}})
	observed, err := topologyFromKitty(tree, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	observed, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, observed)
	if err != nil {
		t.Fatal(err)
	}
	views, _ := findWorkspaceViews(tree, workspace.ID)
	annotateRuntimeViews(observed, views)
	plan := planTopologyReconcile(workspace, observed, views, "")
	if len(plan.MoveTabs) != 2 {
		t.Fatalf("tab regroup plan = %#v, want two moves", plan.MoveTabs)
	}

	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, " ls"):
			return mustJSON(t, tree), "", nil
		case strings.Contains(joined, "detach-tab --match id:101") &&
			!strings.Contains(joined, "--target-tab"):
			// Kitty destroys tab 101 while moving it to a new OS window and
			// recreates it as tab 201. The old id is no longer addressable.
			lead := tree[0].Tabs[1]
			lead.ID = 201
			tree[0].Tabs = tree[0].Tabs[:1]
			tree = append(tree, kittyOSWindow{ID: 2, WMClass: "managed-kitty", Tabs: []kittyTab{lead}})
			return "", "", nil
		case strings.Contains(joined, "detach-tab --match id:100 --target-tab id:201"):
			// A correct second move targets the recreated lead tab.
			trailing := tree[0].Tabs[0]
			trailing.ID = 202
			tree[1].Tabs = append(tree[1].Tabs, trailing)
			tree = tree[1:]
			return "", "", nil
		case strings.Contains(joined, "detach-tab"):
			return "", "tab destination does not exist", errors.New("tab destination does not exist")
		default:
			return "", "", nil
		}
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	attachment := &Attachment{Transport: Transport{Kind: "local"}}
	if err := d.applyTopologyPlan(context.Background(), "unix:/kitty", workspace, attachment, plan); err != nil {
		t.Fatalf("regroup left a partially detached topology: %v", err)
	}
	if len(tree) != 1 || len(tree[0].Tabs) != 2 {
		t.Fatalf("regroup did not converge to one OS window with two tabs: %#v", tree)
	}
}

// Two independent regroup operations in one plan must each use their own
// pane-derived destination. A shared mutable anchor makes the second group
// attach to the first, which is how unrelated windows collapsed together.
func TestIndependentRegroupAnchorsDoNotLeak(t *testing.T) {
	tests := []struct {
		name     string
		desired  []Node
		observed []Node
		moves    func(topologyPlan) map[string]string
		want     map[string]string
	}{
		{
			name: "windows",
			desired: []Node{{Kind: "os-window", Children: []Node{
				{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-0"}, {Kind: "pane", PaneID: "pane-1"}}},
				{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-2"}, {Kind: "pane", PaneID: "pane-3"}}},
			}}},
			observed: []Node{{Kind: "os-window", Children: []Node{
				{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-0"}, {Kind: "pane", PaneID: "pane-2"}}},
				{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-1"}, {Kind: "pane", PaneID: "pane-3"}}},
			}}},
			moves: func(plan topologyPlan) map[string]string {
				result := map[string]string{}
				for _, move := range plan.MoveWindows {
					result[move.PaneID] = move.TargetPaneID
				}
				return result
			},
			want: map[string]string{
				"pane-0": "", "pane-1": "pane-0",
				"pane-2": "", "pane-3": "pane-2",
			},
		},
		{
			name: "tabs",
			desired: []Node{
				{Kind: "os-window", Children: []Node{
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-0"}}},
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-1"}}},
				}},
				{Kind: "os-window", Children: []Node{
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-2"}}},
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-3"}}},
				}},
			},
			observed: []Node{
				{Kind: "os-window", Children: []Node{
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-0"}}},
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-2"}}},
				}},
				{Kind: "os-window", Children: []Node{
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-1"}}},
					{Kind: "tab", Children: []Node{{Kind: "pane", PaneID: "pane-3"}}},
				}},
			},
			moves: func(plan topologyPlan) map[string]string {
				result := map[string]string{}
				for _, move := range plan.MoveTabs {
					result[move.PaneID] = move.TargetPaneID
				}
				return result
			},
			want: map[string]string{
				"pane-0": "", "pane-1": "pane-0",
				"pane-2": "", "pane-3": "pane-2",
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			workspace, paneIDs := newTopologyFuzzWorkspace(4)
			if _, err := installDesiredTopology(workspace, testCase.desired, topologyInstallSystem); err != nil {
				t.Fatal(err)
			}
			stream := topologyFuzzBytes{data: []byte{0}}
			model := newTopologyFuzzKitty(workspace.ID, testCase.observed, &stream)
			client := KittyClient{Runner: model, Command: "kitten-test"}
			daemon := &Daemon{kitty: client, state: StateData{Workspaces: map[string]*Workspace{workspace.ID: workspace}}}
			attachment := &Attachment{Transport: Transport{Kind: "local"}}

			observed, views, err := observeWorkspaceTopology(context.Background(), client, "unix:/kitty", workspace)
			if err != nil {
				t.Fatal(err)
			}
			plan := planTopologyReconcile(workspace, observed, views, "")
			if got := testCase.moves(plan); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("independent regroup plan = %#v, want %#v", got, testCase.want)
			}
			if err := daemon.applyTopologyPlan(context.Background(), "unix:/kitty", workspace, attachment, plan); err != nil {
				t.Fatal(err)
			}
			observed, _, err = observeWorkspaceTopology(context.Background(), client, "unix:/kitty", workspace)
			if err != nil {
				t.Fatal(err)
			}
			if !topologyMatchesDesired(workspace, observed) {
				t.Fatalf("regroup converged to the wrong groups; panes=%v tree=%#v", paneIDs, model.tree)
			}
		})
	}
}

func TestUnknownKittyTabTitleIsNotPinned(t *testing.T) {
	workspace, _ := newTopologyFuzzWorkspace(1)
	if _, err := installDesiredTopology(workspace, []Node{{
		Kind: "os-window", Children: []Node{{
			Kind: "tab", Title: "wanted", Children: []Node{{Kind: "pane", PaneID: "pane-0"}},
		}},
	}}, topologyInstallSystem); err != nil {
		t.Fatal(err)
	}
	observed := cloneNodes(workspace.Topology.Roots)
	known := false
	observed[0].Children[0].Title = "kitty-derived-title"
	observed[0].Children[0].TitleKnown = &known
	views := map[string]RuntimeView{
		"pane-0": {PaneID: "pane-0", WindowID: 1, TabID: 2, OSWindowID: 3, Ready: true},
	}
	plan := planTopologyReconcile(workspace, observed, views, "")
	if len(plan.TabTitles) != 0 {
		t.Fatalf("unknown old-Kitty title produced a permanent title action: %#v", plan.TabTitles)
	}
}

// Backoff must grow, stay bounded, and jitter within its band, so repeated
// failures cannot turn into a hot loop against Kitty.
func TestBackoffScheduleGrowsAndCaps(t *testing.T) {
	previousCeiling := time.Duration(0)
	for attempts := 1; attempts <= 12; attempts++ {
		ceiling := backoffBase << (attempts - 1)
		if ceiling > backoffCap || ceiling <= 0 {
			ceiling = backoffCap
		}
		for i := 0; i < 50; i++ {
			delay := backoffDelay(attempts)
			if delay < ceiling/2 || delay > ceiling {
				t.Fatalf("attempt %d delay %v outside [%v, %v]", attempts, delay, ceiling/2, ceiling)
			}
			if delay > backoffCap {
				t.Fatalf("attempt %d delay %v exceeds cap %v", attempts, delay, backoffCap)
			}
		}
		if ceiling < previousCeiling {
			t.Fatalf("attempt %d ceiling %v regressed from %v", attempts, ceiling, previousCeiling)
		}
		previousCeiling = ceiling
	}
}

// A repeatedly failing endpoint must be rate limited rather than retried on
// every trigger.
func TestFailingReconcileBacksOffInsteadOfSpinning(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:/kitty"
	d.noteTopologyFailure(endpoint, 1)
	d.noteTopologyFailure(endpoint, 1)
	ready, wait := d.topologyBackoffReady(endpoint)
	if ready || wait <= 0 {
		t.Fatalf("expected an open backoff window, got ready=%v wait=%v", ready, wait)
	}
	d.clearTopologyBackoff(endpoint)
	if ready, _ := d.topologyBackoffReady(endpoint); !ready {
		t.Fatal("clearing backoff must allow an immediate retry")
	}
}

// Completing a reconcile must not shorten a settle window that something else
// deliberately installed.
func TestReconcileCompletionDoesNotShortenCaptureSuppression(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:/kitty"
	d.extendTopologyCaptureSuppression(endpoint, 30*time.Second)
	d.topologyMu.Lock()
	d.captureHold[endpoint]++
	d.topologyMu.Unlock()
	d.topologyMu.Lock()
	d.captureHold[endpoint]--
	if d.captureHold[endpoint] == 0 {
		delete(d.captureHold, endpoint)
	}
	d.topologyMu.Unlock()
	if !d.topologyCaptureSuppressed(endpoint) {
		t.Fatal("finishing a reconcile discarded the explicit settle window")
	}
}
