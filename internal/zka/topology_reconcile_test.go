package zka

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReplacementCleanupNeverClosesTopologyPendingPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	desired := firstPane(workspace)
	allocated, err := d.allocatePane(workspace.ID, "concurrent:add", "")
	if err != nil {
		t.Fatal(err)
	}
	pending := allocated.Pane
	tree := []kittyOSWindow{{ID: 1, Tabs: []kittyTab{{ID: 2, Windows: []kittyWindow{
		{ID: 11, UserVars: map[string]string{"zka_workspace": workspace.ID, "zka_pane": desired.ID, "zka_ready": "1"}},
		{ID: 12, UserVars: map[string]string{"zka_workspace": workspace.ID, "zka_pane": pending.ID, "zka_ready": "1"}},
	}}}}}
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if len(args) != 0 && args[len(args)-1] == "ls" {
			return mustJSON(t, tree), "", nil
		}
		return "", "", nil
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"
	if err := d.closeReplacementViews(context.Background(), "unix:/kitty", workspace.ID, nil, map[int64]bool{11: true}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "close-window") {
			t.Fatalf("replacement cleanup closed a pending user pane: %#v", call.Args)
		}
	}
}

func TestReplacementWaitYieldsToConcurrentPendingPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	desired := firstPane(workspace)
	allocated, err := d.allocatePane(workspace.ID, "concurrent:add", "")
	if err != nil {
		t.Fatal(err)
	}
	pending := allocated.Pane
	tree := []kittyOSWindow{{ID: 1, Tabs: []kittyTab{{ID: 2, Windows: []kittyWindow{
		{ID: 11, UserVars: map[string]string{"zka_workspace": workspace.ID, "zka_pane": desired.ID, "zka_ready": "1"}},
		{ID: 12, UserVars: map[string]string{"zka_workspace": workspace.ID, "zka_pane": pending.ID, "zka_ready": "1"}},
	}}}}}
	runner := &fakeRunner{handler: func(_ context.Context, _ string, _ ...string) (string, string, error) {
		return mustJSON(t, tree), "", nil
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"
	_, err = d.waitForReplacementViews(context.Background(), "unix:/kitty", workspace, map[int64]bool{11: true}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "pending pane appeared") {
		t.Fatalf("replacement wait error = %v", err)
	}
}
