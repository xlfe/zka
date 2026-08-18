package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FuzzTopologyReconcileConverges exercises the reconciler as a state machine,
// rather than fuzzing serialized trees that are overwhelmingly invalid. Both
// desired and observed states contain exactly the same panes, but independently
// vary pane order, tab grouping, OS-window grouping, focus, and activity. The
// Kitty model changes runtime tab ids after detach operations, as real Kitty
// does. Presentation has a separate target below so its failures cannot mask
// structural counterexamples.
//
// The properties are:
//   - reconciliation never addresses a runtime object that a prior action
//     destroyed;
//   - every mutation preserves the pane set exactly;
//   - repeated plans reach the desired logical topology and then become silent.
func FuzzTopologyReconcileConverges(f *testing.F) {
	for _, seed := range [][]byte{
		{0},
		{3, 2, 2, 2, 1, 1, 1, 0, 0, 0, 3, 3},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		stream := topologyFuzzBytes{data: input}
		paneCount := 1 + int(stream.next()%4)
		workspace, paneIDs := newTopologyFuzzWorkspace(paneCount)

		desired := fuzzLogicalTopology(&stream, paneIDs, false)
		if _, err := installDesiredTopology(workspace, desired, topologyInstallSystem); err != nil {
			t.Fatalf("install generated desired topology: %v\ninput=%x\ntopology=%#v", err, input, desired)
		}
		observedShape := fuzzLogicalTopology(&stream, paneIDs, false)
		model := newTopologyFuzzKitty(workspace.ID, observedShape, &stream)

		focusedPane := ""
		if stream.next()%2 != 0 {
			focusedPane = paneIDs[int(stream.next())%len(paneIDs)]
		}
		exerciseTopologyFuzz(t, input, workspace, paneIDs, model, focusedPane)
	})
}

// FuzzTopologyPresentationBecomesSilent holds pane membership and grouping
// fixed while varying enforceable and runtime-only presentation. It is kept
// separate from structural fuzzing so a title/layout oscillation cannot mask a
// stale-id or partial-mutation counterexample.
func FuzzTopologyPresentationBecomesSilent(f *testing.F) {
	for _, seed := range [][]byte{{0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		stream := topologyFuzzBytes{data: input}
		paneCount := 1 + int(stream.next()%4)
		workspace, paneIDs := newTopologyFuzzWorkspace(paneCount)
		desired := fuzzLogicalTopology(&stream, paneIDs, true)
		if _, err := installDesiredTopology(workspace, desired, topologyInstallSystem); err != nil {
			t.Fatalf("install generated desired topology: %v\ninput=%x\ntopology=%#v", err, input, desired)
		}
		observedShape := fuzzTopologyPresentation(&stream, desired)
		model := newTopologyFuzzKitty(workspace.ID, observedShape, &stream)
		focusedPane := ""
		if stream.next()%2 != 0 {
			focusedPane = paneIDs[int(stream.next())%len(paneIDs)]
		}
		exerciseTopologyFuzz(t, input, workspace, paneIDs, model, focusedPane)
	})
}

// FuzzTopologyApplyingStateCannotPublishCapturedLayout exercises the safety
// boundary around arbitrary but complete intermediate trees. Reconciliation
// persists "applying" before its first Kitty mutation, so neither a concurrent
// watcher capture nor a delayed ready declaration may publish what it sees
// until verification has converged on the original target.
func FuzzTopologyApplyingStateCannotPublishCapturedLayout(f *testing.F) {
	for _, seed := range [][]byte{{0}, {3, 2, 1, 0, 2, 3, 1}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		stream := topologyFuzzBytes{data: input}
		paneCount := 2 + int(stream.next()%3)
		workspace, paneIDs := newTopologyFuzzWorkspace(paneCount)
		desired := fuzzLogicalTopology(&stream, paneIDs, false)
		if _, err := installDesiredTopology(workspace, desired, topologyInstallSystem); err != nil {
			t.Fatal(err)
		}
		captured := fuzzLogicalTopology(&stream, paneIDs, false)
		stable, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, captured)
		if err != nil {
			t.Fatal(err)
		}
		if topologyStructureEqual(stable, workspace.Topology.Roots) {
			if len(workspace.Topology.Roots) == 1 && len(workspace.Topology.Roots[0].Children) == 1 {
				captured = []Node{{Kind: "os-window"}}
				for _, paneID := range paneIDs {
					captured[0].Children = append(captured[0].Children, Node{
						Kind: "tab", Children: []Node{{Kind: "pane", PaneID: paneID}},
					})
				}
			} else {
				children := make([]Node, 0, len(paneIDs))
				for _, paneID := range paneIDs {
					children = append(children, Node{Kind: "pane", PaneID: paneID})
				}
				captured = []Node{{Kind: "os-window", Children: []Node{{Kind: "tab", Children: children}}}}
			}
			stable, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, captured)
			if err != nil {
				t.Fatal(err)
			}
			if topologyStructureEqual(stable, workspace.Topology.Roots) {
				t.Skip("fallback generated the existing structure")
			}
		}

		views := viewsForPanes(paneIDs...)
		attachment := &Attachment{
			ID: "local", Status: AttachmentPreparing, ReconcileStatus: TopologyReconcileApplying,
			ReconcileTargetGeneration: workspace.Topology.Generation,
			AppliedTopologyGeneration: workspace.Topology.Generation,
			AppliedTopologyDigest:     workspace.Topology.Digest,
			ObservedTopology:          topologyIdentity(workspace.Topology.Roots),
			Transport:                 Transport{Kind: "local"}, Endpoint: "unix:/fuzz", Views: views,
		}
		workspace.Attachments[attachment.ID] = attachment
		beforeGeneration, beforeDigest := workspace.Topology.Generation, workspace.Topology.Digest
		daemon := &Daemon{state: StateData{Workspaces: map[string]*Workspace{workspace.ID: workspace}}}
		manifest := manifestForPanes(workspace, paneIDs...)
		manifest.Topology = captured
		request := manifestUpdateRequest{
			Workspace: workspace.ID, Attachment: attachment.ID,
			Manifest: manifest, Views: views,
		}
		populateManifestSource(&request, workspace, attachment, topologyUpdateUserCapture)
		// Also model a capture request queued just before applying was persisted.
		if stream.next()%2 != 0 {
			request.SourceStatus = AttachmentReady
			request.SourceReconcileStatus = TopologyReconcileReady
		}

		if _, err := daemon.updateManifest(request); !errors.Is(err, errStructuralPublicationRefused) {
			t.Fatalf("applying capture publication error = %v\ninput=%x\ndesired=%#v\ncaptured=%#v",
				err, input, workspace.Topology.Roots, captured)
		}
		if workspace.Topology.Generation != beforeGeneration || workspace.Topology.Digest != beforeDigest {
			t.Fatalf("refused applying capture changed desired topology\ninput=%x", input)
		}
	})
}

func newTopologyFuzzWorkspace(paneCount int) (*Workspace, []string) {
	paneIDs := make([]string, paneCount)
	workspace := &Workspace{
		ID: "workspace", Name: "fuzz", Panes: map[string]*Pane{},
		Attachments: map[string]*Attachment{},
	}
	now := time.Unix(0, 0).UTC()
	for index := range paneIDs {
		paneID := fmt.Sprintf("pane-%d", index)
		paneIDs[index] = paneID
		workspace.Panes[paneID] = &Pane{
			ID: paneID, Position: index, Title: "shell", State: StateUnknown,
			Phase: PaneAdmitted, PhaseAt: now, Backend: BackendRef{Kind: "zmx", Ref: paneID},
			CreatedAt: now, UpdatedAt: now,
		}
	}
	return workspace, paneIDs
}

func exerciseTopologyFuzz(
	t *testing.T,
	input []byte,
	workspace *Workspace,
	paneIDs []string,
	model *topologyFuzzKitty,
	focusedPane string,
) {
	t.Helper()
	client := KittyClient{Runner: model, Command: "kitten-fuzz"}
	daemon := &Daemon{kitty: client, state: StateData{Workspaces: map[string]*Workspace{workspace.ID: workspace}}}
	attachment := &Attachment{Transport: Transport{Kind: "local"}}
	for pass := 0; pass < 16; pass++ {
		observed, views, err := observeWorkspaceTopology(context.Background(), client, "unix:/fuzz", workspace)
		if err != nil {
			t.Fatalf("pass %d observe: %v\ninput=%x\ntree=%#v", pass, err, input, model.tree)
		}
		assertFuzzPaneSet(t, input, model.tree, paneIDs)
		plan := planTopologyReconcile(workspace, observed, views, focusedPane)
		if plan.empty() {
			if !topologyMatchesDesired(workspace, observed) {
				t.Fatalf("pass %d produced an empty plan for divergent topology\ninput=%x\ndesired=%#v\nobserved=%#v",
					pass, input, workspace.Topology.Roots, observed)
			}
			return
		}
		if err := daemon.applyTopologyPlan(context.Background(), "unix:/fuzz", workspace, attachment, plan); err != nil {
			t.Fatalf("pass %d apply %#v: %v\ninput=%x\ntree=%#v", pass, plan, err, input, model.tree)
		}
		assertFuzzPaneSet(t, input, model.tree, paneIDs)
	}
	t.Fatalf("reconciliation did not become silent within 16 passes\ninput=%x\ndesired=%#v\ntree=%#v",
		input, workspace.Topology.Roots, model.tree)
}

type topologyFuzzBytes struct {
	data  []byte
	index int
}

func (s *topologyFuzzBytes) next() byte {
	if len(s.data) == 0 {
		return 0
	}
	value := s.data[s.index%len(s.data)]
	s.index++
	return value
}

// fuzzLogicalTopology turns a pane permutation and a sequence of three-way
// separators into a valid hierarchy: same tab, new tab, or new OS window.
func fuzzLogicalTopology(stream *topologyFuzzBytes, paneIDs []string, presentation bool) []Node {
	ordered := append([]string(nil), paneIDs...)
	for index := len(ordered) - 1; index > 0; index-- {
		swap := int(stream.next()) % (index + 1)
		ordered[index], ordered[swap] = ordered[swap], ordered[index]
	}
	newTab := func(paneID string) Node {
		layouts := [...]string{"splits", "grid", "tall", "stack"}
		titles := [...]string{"", "", "named", "review"}
		layout, title := "splits", ""
		if presentation {
			layout = layouts[int(stream.next())%len(layouts)]
			title = titles[int(stream.next())%len(titles)]
		}
		return Node{
			Kind: "tab", Layout: layout, Title: title,
			Children: []Node{{Kind: "pane", PaneID: paneID}},
		}
	}
	roots := []Node{{Kind: "os-window", Class: "managed-kitty", Children: []Node{newTab(ordered[0])}}}
	for _, paneID := range ordered[1:] {
		root := &roots[len(roots)-1]
		switch stream.next() % 3 {
		case 0:
			tab := &root.Children[len(root.Children)-1]
			tab.Children = append(tab.Children, Node{Kind: "pane", PaneID: paneID})
		case 1:
			root.Children = append(root.Children, newTab(paneID))
		case 2:
			roots = append(roots, Node{
				Kind: "os-window", Class: "managed-kitty", Children: []Node{newTab(paneID)},
			})
		}
	}
	return roots
}

func fuzzTopologyPresentation(stream *topologyFuzzBytes, desired []Node) []Node {
	observed := cloneNodes(desired)
	layouts := [...]string{"splits", "grid", "tall", "stack"}
	titles := [...]string{"", "other", "named", "review"}
	for rootIndex := range observed {
		for tabIndex := range observed[rootIndex].Children {
			tab := &observed[rootIndex].Children[tabIndex]
			tab.Layout = layouts[int(stream.next())%len(layouts)]
			tab.Title = titles[int(stream.next())%len(titles)]
		}
	}
	return observed
}

func assertFuzzPaneSet(t *testing.T, input []byte, tree []kittyOSWindow, paneIDs []string) {
	t.Helper()
	seen := map[string]int{}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				seen[window.UserVars["zka_pane"]]++
			}
		}
	}
	if len(seen) != len(paneIDs) {
		t.Fatalf("pane set changed: got %#v, want %v\ninput=%x", seen, paneIDs, input)
	}
	for _, paneID := range paneIDs {
		if seen[paneID] != 1 {
			t.Fatalf("pane %s occurs %d times\ninput=%x\ntree=%#v", paneID, seen[paneID], input, tree)
		}
	}
}

// topologyFuzzKitty implements the subset of Kitty remote control used by a
// topology plan. Runtime window ids survive moves, while every moved or newly
// created tab receives a fresh id. A stale match or target is an error.
type topologyFuzzKitty struct {
	tree              []kittyOSWindow
	nextOS, nextTab   int64
	renumberMovedTabs bool
}

func newTopologyFuzzKitty(workspaceID string, roots []Node, stream *topologyFuzzBytes) *topologyFuzzKitty {
	model := &topologyFuzzKitty{nextOS: 100, nextTab: 1000, renumberMovedTabs: true}
	windowID := int64(10)
	for rootIndex, root := range roots {
		osWindow := kittyOSWindow{
			ID: int64(rootIndex + 1), WMClass: "managed-kitty",
		}
		activeTab := int(stream.next()) % len(root.Children)
		for tabIndex, node := range root.Children {
			named := node.Title != ""
			tab := kittyTab{
				ID: model.nextTab, Title: node.Title, TitleOverridden: &named,
				Layout: node.Layout, Enabled: []string{"fat", "grid", "horizontal", "splits", "stack", "tall", "vertical"},
				IsActive: tabIndex == activeTab,
			}
			model.nextTab++
			activeWindow := int(stream.next()) % len(node.Children)
			for paneIndex, pane := range node.Children {
				windowID++
				tab.Windows = append(tab.Windows, kittyWindow{
					ID: windowID, Title: "shell", IsActive: paneIndex == activeWindow,
					UserVars: map[string]string{
						"zka_workspace": workspaceID, "zka_pane": pane.PaneID, "zka_ready": "1",
					},
					Cmdline: []string{"zka", "pane", "--workspace", workspaceID},
				})
			}
			refreshFuzzLayoutState(&tab)
			osWindow.Tabs = append(osWindow.Tabs, tab)
		}
		model.tree = append(model.tree, osWindow)
	}
	// Kitty has at most one focused window process-wide. Randomly choose one
	// valid focus chain, or leave the process unfocused.
	if stream.next()%2 != 0 {
		focusIndex := int(stream.next()) % len(workspacePaneIDsFromTree(model.tree))
		focusPane := workspacePaneIDsFromTree(model.tree)[focusIndex]
		for rootIndex := range model.tree {
			for tabIndex := range model.tree[rootIndex].Tabs {
				for windowIndex := range model.tree[rootIndex].Tabs[tabIndex].Windows {
					window := &model.tree[rootIndex].Tabs[tabIndex].Windows[windowIndex]
					if window.UserVars["zka_pane"] == focusPane {
						window.IsFocused = true
						model.tree[rootIndex].Tabs[tabIndex].IsFocused = true
						model.tree[rootIndex].IsFocused = true
					}
				}
			}
		}
	}
	return model
}

func workspacePaneIDsFromTree(tree []kittyOSWindow) []string {
	var paneIDs []string
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				paneIDs = append(paneIDs, window.UserVars["zka_pane"])
			}
		}
	}
	return paneIDs
}

func (k *topologyFuzzKitty) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	command := fuzzRCCommand(args)
	switch command {
	case "ls":
		encoded, err := json.Marshal(k.tree)
		return string(encoded), "", err
	case "detach-tab":
		return "", "", k.detachTab(args)
	case "detach-window":
		return "", "", k.detachWindow(args)
	case "set-enabled-layouts":
		return "", "", k.setEnabledLayouts(args)
	case "goto-layout":
		return "", "", k.gotoLayout(args)
	case "set-tab-title":
		return "", "", k.setTabTitle(args)
	case "focus-window":
		return "", "", k.focusWindow(args)
	case "close-window", "launch":
		return "", "unexpected pane-set mutation", fmt.Errorf("unexpected %s for an unchanged pane set", command)
	default:
		return "", "unknown command", fmt.Errorf("unsupported fuzz Kitty command %q: %v", command, args)
	}
}

func fuzzRCCommand(args []string) string {
	commands := map[string]bool{
		"ls": true, "detach-tab": true, "detach-window": true,
		"set-enabled-layouts": true, "goto-layout": true, "set-tab-title": true,
		"focus-window": true, "close-window": true, "launch": true,
	}
	for _, arg := range args {
		if commands[arg] {
			return arg
		}
	}
	return ""
}

func fuzzRCID(args []string, option string) (int64, bool) {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != option || !strings.HasPrefix(args[index+1], "id:") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[index+1], "id:"), 10, 64)
		return id, err == nil
	}
	return 0, false
}

func (k *topologyFuzzKitty) findTab(id int64) (int, int, bool) {
	for rootIndex := range k.tree {
		for tabIndex := range k.tree[rootIndex].Tabs {
			if k.tree[rootIndex].Tabs[tabIndex].ID == id {
				return rootIndex, tabIndex, true
			}
		}
	}
	return 0, 0, false
}

func (k *topologyFuzzKitty) findWindow(id int64) (int, int, int, bool) {
	for rootIndex := range k.tree {
		for tabIndex := range k.tree[rootIndex].Tabs {
			for windowIndex := range k.tree[rootIndex].Tabs[tabIndex].Windows {
				if k.tree[rootIndex].Tabs[tabIndex].Windows[windowIndex].ID == id {
					return rootIndex, tabIndex, windowIndex, true
				}
			}
		}
	}
	return 0, 0, 0, false
}

func (k *topologyFuzzKitty) detachTab(args []string) error {
	sourceID, ok := fuzzRCID(args, "--match")
	if !ok {
		return fmt.Errorf("detach-tab has no source id")
	}
	rootIndex, tabIndex, ok := k.findTab(sourceID)
	if !ok {
		return fmt.Errorf("tab source %d does not exist", sourceID)
	}
	targetID, targeted := fuzzRCID(args, "--target-tab")
	if targeted {
		if targetID == sourceID {
			return fmt.Errorf("tab %d cannot target itself", sourceID)
		}
		if _, _, found := k.findTab(targetID); !found {
			return fmt.Errorf("tab destination %d does not exist", targetID)
		}
	}
	tab := k.tree[rootIndex].Tabs[tabIndex]
	k.tree[rootIndex].Tabs = append(k.tree[rootIndex].Tabs[:tabIndex], k.tree[rootIndex].Tabs[tabIndex+1:]...)
	if len(k.tree[rootIndex].Tabs) == 0 {
		k.tree = append(k.tree[:rootIndex], k.tree[rootIndex+1:]...)
	}
	if k.renumberMovedTabs {
		tab.ID = k.nextTab
		k.nextTab++
	}
	refreshFuzzLayoutState(&tab)

	if targeted {
		targetRoot, _, found := k.findTab(targetID)
		if !found {
			return fmt.Errorf("tab destination %d does not exist", targetID)
		}
		k.tree[targetRoot].Tabs = append(k.tree[targetRoot].Tabs, tab)
		return nil
	}
	k.tree = append(k.tree, kittyOSWindow{ID: k.nextOS, WMClass: "managed-kitty", Tabs: []kittyTab{tab}})
	k.nextOS++
	return nil
}

func (k *topologyFuzzKitty) detachWindow(args []string) error {
	windowID, ok := fuzzRCID(args, "--match")
	if !ok {
		return fmt.Errorf("detach-window has no source id")
	}
	rootIndex, tabIndex, windowIndex, ok := k.findWindow(windowID)
	if !ok {
		return fmt.Errorf("window source %d does not exist", windowID)
	}
	targetID, targeted := fuzzRCID(args, "--target-tab")
	if targeted {
		if _, _, found := k.findTab(targetID); !found {
			return fmt.Errorf("window destination tab %d does not exist", targetID)
		}
	}
	window := k.tree[rootIndex].Tabs[tabIndex].Windows[windowIndex]
	tab := &k.tree[rootIndex].Tabs[tabIndex]
	tab.Windows = append(tab.Windows[:windowIndex], tab.Windows[windowIndex+1:]...)
	refreshFuzzLayoutState(tab)

	if targeted {
		if len(tab.Windows) == 0 {
			k.tree[rootIndex].Tabs = append(k.tree[rootIndex].Tabs[:tabIndex], k.tree[rootIndex].Tabs[tabIndex+1:]...)
			if len(k.tree[rootIndex].Tabs) == 0 {
				k.tree = append(k.tree[:rootIndex], k.tree[rootIndex+1:]...)
			}
		}
		targetRoot, targetTab, found := k.findTab(targetID)
		if !found {
			return fmt.Errorf("window destination tab %d does not exist", targetID)
		}
		destination := &k.tree[targetRoot].Tabs[targetTab]
		destination.Windows = append(destination.Windows, window)
		refreshFuzzLayoutState(destination)
		return nil
	}

	// --target-tab new creates a sibling tab in the source OS window. Keep the
	// OS container alive even when the source tab held only this window.
	if len(tab.Windows) == 0 {
		k.tree[rootIndex].Tabs = append(k.tree[rootIndex].Tabs[:tabIndex], k.tree[rootIndex].Tabs[tabIndex+1:]...)
	}
	named := false
	newTab := kittyTab{
		ID: k.nextTab, Layout: "splits", TitleOverridden: &named,
		Enabled: []string{"fat", "grid", "horizontal", "splits", "stack", "tall", "vertical"},
		Windows: []kittyWindow{window},
	}
	k.nextTab++
	refreshFuzzLayoutState(&newTab)
	k.tree[rootIndex].Tabs = append(k.tree[rootIndex].Tabs, newTab)
	return nil
}

func (k *topologyFuzzKitty) setEnabledLayouts(args []string) error {
	tabID, ok := fuzzRCID(args, "--match")
	if !ok {
		return fmt.Errorf("set-enabled-layouts has no tab id")
	}
	rootIndex, tabIndex, ok := k.findTab(tabID)
	if !ok {
		return fmt.Errorf("layout tab %d does not exist", tabID)
	}
	matchIndex := -1
	for index, arg := range args {
		if arg == "--match" {
			matchIndex = index
			break
		}
	}
	if matchIndex >= 0 && matchIndex+2 < len(args) {
		k.tree[rootIndex].Tabs[tabIndex].Enabled = append([]string(nil), args[matchIndex+2:]...)
	}
	return nil
}

func (k *topologyFuzzKitty) gotoLayout(args []string) error {
	tabID, ok := fuzzRCID(args, "--match")
	if !ok {
		return fmt.Errorf("goto-layout has no tab id")
	}
	rootIndex, tabIndex, ok := k.findTab(tabID)
	if !ok {
		return fmt.Errorf("layout tab %d does not exist", tabID)
	}
	if len(args) == 0 {
		return fmt.Errorf("goto-layout has no layout")
	}
	k.tree[rootIndex].Tabs[tabIndex].Layout = args[len(args)-1]
	return nil
}

func (k *topologyFuzzKitty) setTabTitle(args []string) error {
	tabID, ok := fuzzRCID(args, "--match")
	if !ok {
		return fmt.Errorf("set-tab-title has no tab id")
	}
	rootIndex, tabIndex, ok := k.findTab(tabID)
	if !ok {
		return fmt.Errorf("title tab %d does not exist", tabID)
	}
	title := ""
	for index, arg := range args {
		if arg == "--" && index+1 < len(args) {
			title = args[index+1]
			break
		}
	}
	named := title != ""
	k.tree[rootIndex].Tabs[tabIndex].Title = title
	k.tree[rootIndex].Tabs[tabIndex].TitleOverridden = &named
	return nil
}

func (k *topologyFuzzKitty) focusWindow(args []string) error {
	match := ""
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--match" {
			match = args[index+1]
			break
		}
	}
	for rootIndex := range k.tree {
		k.tree[rootIndex].IsFocused = false
		for tabIndex := range k.tree[rootIndex].Tabs {
			k.tree[rootIndex].Tabs[tabIndex].IsFocused = false
			for windowIndex := range k.tree[rootIndex].Tabs[tabIndex].Windows {
				window := &k.tree[rootIndex].Tabs[tabIndex].Windows[windowIndex]
				window.IsFocused = false
				matches := match == "id:"+strconv.FormatInt(window.ID, 10) ||
					(strings.HasPrefix(match, "var:zka_pane=") && window.UserVars["zka_pane"] == strings.TrimPrefix(match, "var:zka_pane="))
				if matches {
					window.IsFocused = true
					k.tree[rootIndex].Tabs[tabIndex].IsFocused = true
					k.tree[rootIndex].IsFocused = true
				}
			}
		}
	}
	return nil
}

func refreshFuzzLayoutState(tab *kittyTab) {
	groups := make([]string, 0, len(tab.Windows))
	for _, window := range tab.Windows {
		groups = append(groups, fmt.Sprintf(`{"id":%d,"window_ids":[%d]}`, window.ID, window.ID))
	}
	tab.LayoutState = json.RawMessage(fmt.Sprintf(
		`{"all_windows":{"active_group_idx":0,"active_group_history":[],"window_groups":[%s]},"class":"Splits","opts":{}}`,
		strings.Join(groups, ",")))
}
