package zka

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestKittyFocusUsesStablePaneVariable(t *testing.T) {
	runner := &fakeRunner{}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	if err := kitty.FocusPane(context.Background(), "unix:/kitty", "workspace", "pane"); err != nil {
		t.Fatal(err)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "kitten-test" || !strings.Contains(strings.Join(calls[0].Args, "|"), "var:zka_pane=pane") {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestKittyPaneStateTitleRemainsChildControlled(t *testing.T) {
	runner := &fakeRunner{}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	workspace := &Workspace{ID: "workspace"}
	pane := &Pane{ID: "pane", Title: "shell", State: StateUnknown}
	if err := kitty.SetPaneState(context.Background(), "unix:/kitty", RuntimeView{WindowID: 17}, workspace, pane); err != nil {
		t.Fatal(err)
	}

	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %#v", calls)
	}
	joined := strings.Join(calls[1].Args, "|")
	if !strings.Contains(joined, "set-window-title|--temporary|--match|id:17|--|[?] shell") {
		t.Fatalf("pane title is not a temporary Kitty override: %#v", calls[1].Args)
	}
}

func TestKittyCloseWorkspaceDoesNotWaitForFinalWindowResponse(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if !strings.Contains(strings.Join(args, " "), "--no-response") {
			return "", "Error: EOF", errors.New("exit status 1")
		}
		return "", "", nil
	}}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	if err := kitty.CloseWorkspace(context.Background(), "unix:/kitty", "workspace"); err != nil {
		t.Fatalf("close workspace = %v", err)
	}
	calls := runner.Calls()
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].Args, "|"), "close-window|--no-response|--match|var:zka_workspace=workspace") {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestKittyLoadSessionUsesExistingWindowAsActionAnchor(t *testing.T) {
	runner := &fakeRunner{}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	if err := kitty.LoadSession(context.Background(), "unix:/kitty", "/tmp/reconcile.kitty-session", 17); err != nil {
		t.Fatal(err)
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	joined := strings.Join(calls[0].Args, "|")
	if !strings.Contains(joined, "action|--match|id:17|goto_session|/tmp/reconcile.kitty-session") {
		t.Fatalf("load session args = %#v", calls[0].Args)
	}
}

func TestKittyLaunchPaneUsesCanonicalReplicaIdentity(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if len(args) != 0 && args[len(args)-1] == "17" {
			return "", "", nil
		}
		return "17\n", "", nil
	}}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	workspace := &Workspace{ID: "workspace", Panes: map[string]*Pane{}}
	pane := &Pane{ID: "pane", Title: "Agent", CWD: "/origin", State: StateWorking}
	windowID, err := kitty.LaunchPane(context.Background(), "unix:/kitty", workspace, pane,
		Transport{Kind: "ssh", Host: "origin.example"}, "attachment", "os-node", "tab-node", "tab", 9)
	if err != nil {
		t.Fatal(err)
	}
	if windowID != 17 {
		t.Fatalf("window id = %d", windowID)
	}
	var combined string
	for _, call := range runner.Calls() {
		combined += strings.Join(call.Args, "|") + "\n"
	}
	for _, required := range []string{
		"focus-window|--match|id:9",
		"launch|--type=tab",
		"zka_os_window=os-node",
		"zka_tab=tab-node",
		"remote-pane|--origin|origin.example",
	} {
		if !strings.Contains(combined, required) {
			t.Fatalf("launch calls do not contain %q: %s", required, combined)
		}
	}
	if strings.Contains(combined, "--cwd|/origin") {
		t.Fatalf("remote replica leaked origin cwd into local Kitty launch: %s", combined)
	}
}

func TestFindWorkspaceViewsKeepsRuntimeIDsInAttachment(t *testing.T) {
	tree := []kittyOSWindow{{ID: 1, IsFocused: true, Tabs: []kittyTab{{ID: 2, IsActive: true, Windows: []kittyWindow{
		{ID: 9, IsActive: true, UserVars: map[string]string{"zka_workspace": "work", "zka_pane": "pane"}},
		{ID: 10, UserVars: map[string]string{}},
	}}}}}
	views, untagged := findWorkspaceViews(tree, "work")
	if len(views) != 1 || !views["pane"].Focused || views["pane"].Ready || views["pane"].TabID != 2 || len(untagged) != 1 || untagged[0].ID != 10 || untagged[0].Nascent {
		t.Fatalf("views=%#v untagged=%#v", views, untagged)
	}
	tree[0].Tabs[0].Windows[0].UserVars["zka_ready"] = "1"
	views, _ = findWorkspaceViews(tree, "work")
	if !views["pane"].Ready {
		t.Fatalf("explicitly ready view = %#v", views["pane"])
	}
}

func TestTopologyHasLogicalIDsOnly(t *testing.T) {
	tree := []kittyOSWindow{
		{ID: 41, Tabs: []kittyTab{
			{ID: 42, Title: "[!] Work", Layout: "splits", Windows: []kittyWindow{
				{ID: 43, Title: "[✓] Pane", CWD: "/work", UserVars: map[string]string{"zka_workspace": "work", "zka_pane": "pane"}},
			}},
		}},
	}
	topology, err := topologyFromKitty(tree, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got := topology[0].Children[0].Children[0].PaneID; got != "pane" {
		t.Fatalf("pane id = %q", got)
	}
	if topology[0].Children[0].Title != "Work" || topology[0].Children[0].Children[0].Title != "Pane" {
		t.Fatalf("attention marker leaked into topology: %#v", topology)
	}
	encoded := mustJSON(t, topology)
	for _, runtimeID := range []string{`"id":41`, `"id":42`, `"id":43`, `window_id`} {
		if strings.Contains(encoded, runtimeID) {
			t.Fatalf("topology leaked runtime id: %s", encoded)
		}
	}
}

func TestKittyLayoutStateUsesStableTabLocalPaneIDs(t *testing.T) {
	first, err := logicalKittyLayoutState(kittyTab{
		LayoutState: json.RawMessage(`{"pairs":{"horizontal":false,"bias":0.7,"one":50,"two":60},"class":"Splits","opts":{},"all_windows":{"active_group_idx":1,"active_group_history":[50],"window_groups":[{"id":50,"window_ids":[700]},{"id":60,"window_ids":[800]}]}}`),
		Windows:     []kittyWindow{{ID: 700}, {ID: 800}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := logicalKittyLayoutState(kittyTab{
		LayoutState: json.RawMessage(`{"all_windows":{"window_groups":[{"id":5,"window_ids":[17]},{"id":9,"window_ids":[23]}],"active_group_history":[9],"active_group_idx":0},"opts":{},"class":"Splits","pairs":{"two":9,"one":5,"bias":0.7,"horizontal":false}}`),
		Windows:     []kittyWindow{{ID: 17}, {ID: 23}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("logical layout differs across Kitty runtimes:\n%s\n%s", first, second)
	}
	for _, runtimeID := range []string{"700", "800", "50", "60"} {
		if strings.Contains(string(first), runtimeID) {
			t.Fatalf("logical layout retained runtime id %s: %s", runtimeID, first)
		}
	}
	for _, logical := range []string{`"one":1`, `"two":2`, `"window_ids":[1]`, `"window_ids":[2]`} {
		if !strings.Contains(string(first), logical) {
			t.Fatalf("logical layout missing %s: %s", logical, first)
		}
	}
}

// A launch token must survive Kitty's shlex split verbatim, and the "$"
// doubling must survive its expandvars pass.
func TestLaunchTokenQuoting(t *testing.T) {
	value := "a \"quote\" $HOME launch evil"
	quoted := shlexQuote(escapeExpandVars(value))
	tokens, err := shlexSplit(quoted)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("split %q = %#v, %v", quoted, tokens, err)
	}
	if got := unescapeExpandVars(tokens[0]); got != value {
		t.Fatalf("round trip = %q, want %q", got, value)
	}
}

func mustJSON(t testing.TB, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
