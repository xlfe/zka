package zka

import (
	"context"
	"os"
	"strings"
	"testing"
)

func mustParseTemplate(t *testing.T, text string) SessionTemplate {
	t.Helper()
	template, err := ParseSessionTemplate(text)
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func genesisWorkspace(t *testing.T, d *Daemon, name, templateText string) *Workspace {
	t.Helper()
	plan, err := TemplateGenesis(mustParseTemplate(t, templateText), "/work")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := d.createWorkspace(createWorkspaceRequest{
		Name: name, Shell: []string{"fish"}, Panes: plan.Panes,
		Topology: plan.Topology, FocusPane: plan.FocusPane,
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestTemplateGenesisInterpretsStructure(t *testing.T) {
	plan, err := TemplateGenesis(mustParseTemplate(t, strings.Join([]string{
		"new_tab work",
		"layout splits",
		"launch --location default",
		"launch --location vsplit --cwd sub",
		"title Editor",
		"launch",
		"focus",
		"new_os_window",
		"os_window_class helper",
		"new_tab logs",
		"enabled_layouts splits, tall",
		"launch --title tail --cwd /var/log",
	}, "\n")), "/home/felix")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Panes) != 4 {
		t.Fatalf("panes = %#v", plan.Panes)
	}
	if plan.Panes[0].CWD != "/home/felix" || plan.Panes[0].Title != "" {
		t.Fatalf("pane 0 = %#v", plan.Panes[0])
	}
	if plan.Panes[1].CWD != "/home/felix/sub" {
		t.Fatalf("relative --cwd was not resolved: %#v", plan.Panes[1])
	}
	if plan.Panes[2].Title != "Editor" {
		t.Fatalf("pending title was not applied: %#v", plan.Panes[2])
	}
	if plan.Panes[3].Title != "tail" || plan.Panes[3].CWD != "/var/log" {
		t.Fatalf("pane 3 = %#v", plan.Panes[3])
	}
	if plan.FocusPane == nil || *plan.FocusPane != 2 {
		t.Fatalf("focus = %#v", plan.FocusPane)
	}
	if len(plan.Topology) != 2 {
		t.Fatalf("topology = %#v", plan.Topology)
	}
	work := plan.Topology[0]
	if len(work.Tabs) != 1 || work.Tabs[0].Title != "work" || work.Tabs[0].Layout != "splits" {
		t.Fatalf("first os-window = %#v", work)
	}
	if got := work.Tabs[0].Panes; len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("first tab panes = %#v", got)
	}
	logs := plan.Topology[1]
	if logs.Class != "helper" || len(logs.Tabs) != 1 || logs.Tabs[0].Title != "logs" {
		t.Fatalf("second os-window = %#v", logs)
	}
	if got := logs.Tabs[0].EnabledLayouts; len(got) != 2 || got[0] != "splits" || got[1] != "tall" {
		t.Fatalf("enabled layouts = %#v", got)
	}
	if got := logs.Tabs[0].Panes; len(got) != 1 || got[0] != 3 {
		t.Fatalf("second tab panes = %#v", got)
	}
	// Stored options keep split geometry and drop the pane-model fields.
	if !plan.Panes[1].LaunchOptions.Has("--location") {
		t.Fatalf("split geometry was dropped: %#v", plan.Panes[1].LaunchOptions)
	}
	for index, pane := range plan.Panes {
		for _, name := range []string{"--cwd", "--title", "--window-title", "--tab-title"} {
			if pane.LaunchOptions.Has(name) {
				t.Fatalf("pane %d stored pane-model option %s: %#v", index, name, pane.LaunchOptions)
			}
		}
	}
}

func TestTemplateGenesisReplacesEmptyTabLikeKitty(t *testing.T) {
	// Kitty deletes a tab that never received a window, so settings applied to
	// it must die with it rather than leak into the replacement.
	plan, err := TemplateGenesis(mustParseTemplate(t, "layout tall\nnew_tab work\nlaunch\n"), "/work")
	if err != nil {
		t.Fatal(err)
	}
	tab := plan.Topology[0].Tabs[0]
	if tab.Title != "work" || tab.Layout != "" {
		t.Fatalf("replaced tab = %#v", tab)
	}
}

func TestTemplateGenesisDefaultsAndDroppedDirectives(t *testing.T) {
	plan, err := TemplateGenesis(DefaultSessionTemplate(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Panes) != 1 || plan.FocusPane != nil || len(plan.Topology) != 1 {
		t.Fatalf("default plan = %#v", plan)
	}
	// focus before any launch is ignored; view-only directives are accepted.
	plan, err = TemplateGenesis(mustParseTemplate(t,
		"focus\ncd /tmp\nos_window_size 100 100\nresize_window\nfocus_tab 0\nlaunch\n"), "/work")
	if err != nil {
		t.Fatal(err)
	}
	if plan.FocusPane != nil {
		t.Fatalf("focus before any launch was recorded: %#v", plan.FocusPane)
	}
}

func TestTemplateGenesisRejectsUnmaterializableTemplates(t *testing.T) {
	cases := []struct {
		name       string
		template   string
		defaultCWD string
		wantErr    string
	}{
		{"trailing empty tab", "launch\nnew_tab empty", "/work", "no launch"},
		{"empty tab before os window", "launch\nnew_tab empty\nnew_os_window\nlaunch", "/work", "no launch"},
		{"unknown layout", "layout bogus\nlaunch", "/work", "unknown layout"},
		{"unknown enabled layout", "enabled_layouts splits,bogus\nlaunch", "/work", "unknown layout"},
		{"layout state", "set_layout_state {}\nlaunch", "/work", "not supported"},
		{"relative cwd without default", "launch --cwd rel", "", "relative"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := TemplateGenesis(mustParseTemplate(t, testCase.template), testCase.defaultCWD)
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want %q", err, testCase.wantErr)
			}
		})
	}
}

func TestCreateWorkspaceGenesisInstallsTopologyAndManifest(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := genesisWorkspace(t, d, "genesis",
		"new_tab work\nlayout splits\nlaunch --location default\nlaunch --location vsplit\nfocus\n")
	if workspace.Topology.Generation != 1 {
		t.Fatalf("generation = %d", workspace.Topology.Generation)
	}
	if workspace.Topology.Digest != topologyStructuralDigest(workspace.Topology.Roots) {
		t.Fatal("stored digest does not describe stored roots")
	}
	if !nodesEqual(workspace.Manifest.Topology, workspace.Topology.Roots) {
		t.Fatal("manifest topology does not mirror the desired topology")
	}
	if strings.TrimSpace(workspace.Manifest.Session) == "" {
		t.Fatal("manifest session is empty; remote attach would be gated out")
	}
	panes := workspace.SortedPanes()
	if workspace.RestoreFocusPaneID != panes[1].ID {
		t.Fatalf("restore focus = %q, want pane %q", workspace.RestoreFocusPaneID, panes[1].ID)
	}
	for _, pane := range panes {
		if !pane.Admitted() {
			t.Fatalf("pane %s is %s, want admitted", pane.ID, pane.Phase)
		}
	}
	if _, err := RenderAttachmentSession(workspace, Transport{Kind: "ssh", Host: "devbox.example"}, "att"); err != nil {
		t.Fatalf("genesis workspace is not renderable for a remote attachment: %v", err)
	}
}

func TestCreateWorkspaceGenesisDefaultsPaneCWDToHome(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := TemplateGenesis(DefaultSessionTemplate(), "")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := d.createWorkspace(createWorkspaceRequest{
		Name: "home", Shell: []string{"fish"}, Panes: plan.Panes, Topology: plan.Topology,
	})
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := workspace.SortedPanes()[0].CWD; got != home {
		t.Fatalf("pane cwd = %q, want origin home %q", got, home)
	}
}

func TestCreateWorkspaceGenesisRejectsInvalidRequestsAtomically(t *testing.T) {
	focusOutOfRange := 5
	valid := []GenesisOSWindow{{Tabs: []GenesisTab{{Panes: []int{0}}}}}
	cases := []struct {
		name string
		req  createWorkspaceRequest
	}{
		{"pane index out of range", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, Topology: []GenesisOSWindow{{Tabs: []GenesisTab{{Panes: []int{0, 1}}}}}}},
		{"pane placed twice", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}, {CWD: "/w"}}, Topology: []GenesisOSWindow{{Tabs: []GenesisTab{{Panes: []int{0, 0}}}}}}},
		{"pane not placed", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}, {CWD: "/w"}}, Topology: []GenesisOSWindow{{Tabs: []GenesisTab{{Panes: []int{0}}}}}}},
		{"os window without tabs", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, Topology: []GenesisOSWindow{{}}}},
		{"tab without panes", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, Topology: []GenesisOSWindow{{Tabs: []GenesisTab{{}}}}}},
		{"unknown layout", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, Topology: []GenesisOSWindow{{Tabs: []GenesisTab{{Layout: "bogus", Panes: []int{0}}}}}}},
		{"relative pane cwd", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "relative"}}, Topology: valid}},
		{"focus out of range", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, Topology: valid, FocusPane: &focusOutOfRange}},
		{"focus without topology", createWorkspaceRequest{
			Panes: []PaneSpec{{CWD: "/w"}}, FocusPane: &focusOutOfRange}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			d, err := newTestDaemon(t, testRoot(t), quietRunner())
			if err != nil {
				t.Fatal(err)
			}
			testCase.req.Shell = []string{"fish"}
			if _, err := d.createWorkspace(testCase.req); err == nil {
				t.Fatal("invalid genesis request was accepted")
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.state.Workspaces) != 0 {
				t.Fatalf("rejected genesis left state behind: %#v", d.state.Workspaces)
			}
		})
	}
}

func TestCreateWorkspaceTrimsStoredName(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := d.createWorkspace(createWorkspaceRequest{Name: "  padded  ", Shell: []string{"fish"}, Panes: []PaneSpec{{}}})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "padded" {
		t.Fatalf("stored name = %q", workspace.Name)
	}
	if _, err := d.createWorkspace(createWorkspaceRequest{Name: "padded ", Shell: []string{"fish"}, Panes: []PaneSpec{{}}}); err == nil {
		t.Fatal("differently padded duplicate name was accepted")
	}
}

func TestGenesisWorkspaceSurvivesDaemonRestartUnchanged(t *testing.T) {
	paths := testPaths(testRoot(t))
	runner := newLifecycleRunner()
	first, err := newTestDaemonAtPaths(t, paths, runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := genesisWorkspace(t, first, "genesis", "new_tab work\nlayout splits\nlaunch\nlaunch\n")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newTestDaemonAtPaths(t, paths, runner)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Topology.Generation != workspace.Topology.Generation || got.Topology.Digest != workspace.Topology.Digest {
		t.Fatalf("restart churned the genesis topology: %d/%s -> %d/%s",
			workspace.Topology.Generation, workspace.Topology.Digest, got.Topology.Generation, got.Topology.Digest)
	}
	if !nodesEqual(got.Topology.Roots, workspace.Topology.Roots) {
		t.Fatal("restart rewrote the genesis roots")
	}
	result, err := restarted.reconcileBackends(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Marked) != 0 || len(result.Deleted) != 0 {
		t.Fatalf("restarted reaper touched the genesis workspace: %#v", result)
	}
}

// The key convergence property: the first real capture of a genesis-built
// workspace reproduces the same structural digest, so the generation never
// moves and the attachment goes Ready at generation 1.
func TestGenesisTopologyConvergesOnFirstCapture(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := genesisWorkspace(t, d, "genesis", strings.Join([]string{
		"new_tab work",
		"layout splits",
		"launch --location default",
		"launch --location vsplit",
		"focus",
		"new_os_window",
		"new_tab logs",
		"launch --title tail",
	}, "\n"))
	session := workspace.Manifest.Session
	kitty := &fakeKitty{}
	if err := kitty.LoadSession(session); err != nil {
		t.Fatalf("kitty rejected the genesis session: %v\n%s", err, session)
	}
	tree := kitty.LS()
	observed, err := topologyFromKitty(tree, workspace.ID)
	if err != nil {
		t.Fatalf("read topology back: %v\n%s", err, session)
	}
	stable, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !topologyMatchesDesired(workspace, stable) {
		t.Fatalf("genesis topology is not a fixed point through Kitty\nsession:\n%s\ndesired: %#v\nobserved: %#v",
			session, workspace.Topology.Roots, stable)
	}

	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/local",
	})
	if err != nil {
		t.Fatal(err)
	}
	views, _ := findWorkspaceViews(tree, workspace.ID)
	for paneID, view := range views {
		view.Ready = true
		views[paneID] = view
	}
	updated, err := d.updateManifest(manifestUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Manifest: Manifest{KittyVersion: "kitty 0.47.4", Session: session, Topology: observed},
		Views:    views,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Topology.Generation != 1 {
		t.Fatalf("first capture bumped the generation to %d", updated.Topology.Generation)
	}
	if got := updated.Attachments["local"].Status; got != AttachmentReady {
		t.Fatalf("attachment status after first capture = %q", got)
	}
}

func TestCreateWorkspaceCreationKeyReplaySafety(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	key := "00112233445566778899aabbccddeeff"
	first, err := d.createWorkspace(createWorkspaceRequest{Name: "one", Shell: []string{"fish"}, Panes: []PaneSpec{{}}, CreationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Attachments) != 0 {
		t.Fatalf("created workspace is not dormant: %#v", first.Attachments)
	}
	// The key is a replay token, not a content hash: the same key returns the
	// original workspace even under a different name, checked before name
	// uniqueness so a replay can never collide with itself.
	replayed, err := d.createWorkspace(createWorkspaceRequest{Name: "two", Shell: []string{"fish"}, Panes: []PaneSpec{{}}, CreationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("replay created %s, want %s", replayed.ID, first.ID)
	}
	d.mu.Lock()
	count := len(d.state.Workspaces)
	d.mu.Unlock()
	if count != 1 {
		t.Fatalf("workspace count = %d", count)
	}
	if _, err := d.createWorkspace(createWorkspaceRequest{Name: "three", Shell: []string{"fish"}, Panes: []PaneSpec{{}}, CreationKey: "short"}); err == nil ||
		!strings.Contains(err.Error(), "creation key") {
		t.Fatalf("malformed key error = %v", err)
	}
}

func TestGenesisWorkspaceCanBeRolledBackAndKilled(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	rollback := genesisWorkspace(t, d, "rollback", "launch\n")
	if err := d.deleteWorkspace(rollback.ID); err != nil {
		t.Fatalf("rollback delete refused a dormant genesis workspace: %v", err)
	}
	killed := genesisWorkspace(t, d, "killed", "launch\n")
	if _, err := d.killWorkspace(context.Background(), killed.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, getErr := d.getWorkspace(killed.ID)
		return getErr != nil
	})
}
