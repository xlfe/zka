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

func TestFindWorkspaceViewsKeepsRuntimeIDsInAttachment(t *testing.T) {
	tree := []kittyOSWindow{{ID: 1, IsFocused: true, Tabs: []kittyTab{{ID: 2, IsActive: true, Windows: []kittyWindow{
		{ID: 9, IsActive: true, UserVars: map[string]string{"zka_workspace": "work", "zka_pane": "pane"}},
		{ID: 10, UserVars: map[string]string{}},
	}}}}}
	views, untagged := findWorkspaceViews(tree, "work")
	if len(views) != 1 || !views["pane"].Focused || views["pane"].Ready || views["pane"].TabID != 2 || len(untagged) != 1 || untagged[0] != 10 {
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

func TestQuoteKitty(t *testing.T) {
	got := quoteKitty("a \"quote\" $HOME\nlaunch evil")
	if !strings.Contains(got, `$$HOME`) || !strings.Contains(got, ` launch evil`) || strings.ContainsRune(got, '\n') || !strings.HasPrefix(got, `"`) {
		t.Fatalf("quoted = %q", got)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
