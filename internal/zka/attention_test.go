package zka

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestBuildAttentionSnapshotOrdersLiveItemsAndSuppressesFocusedDone(t *testing.T) {
	base := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	state := newStateData()
	state.AttentionPaused = true
	state.Workspaces["local"] = &Workspace{
		ID: "local", Name: "example-project", Origin: Host{Name: "devbox.example"},
		Panes: map[string]*Pane{
			"blocked-new":  {ID: "blocked-new", Title: "tests", Agent: "codex", State: StateBlocked, Evidence: Evidence{Source: "codex", Event: "permission_request", Detail: "approve command", Timestamp: base.Add(2 * time.Minute)}},
			"blocked-old":  {ID: "blocked-old", Title: "build", Agent: "codex", State: StateBlocked, Evidence: Evidence{Source: "codex", Event: "permission_request", Timestamp: base}},
			"done-focused": {ID: "done-focused", State: StateDone, Evidence: Evidence{Timestamp: base}},
			"working":      {ID: "working", State: StateWorking, Evidence: Evidence{Timestamp: base}},
		},
		Attachments: map[string]*Attachment{"local": {
			Endpoint: "unix:/tmp/kitty.sock", Status: AttachmentReady,
			Views: map[string]RuntimeView{
				"blocked-new":  {PaneID: "blocked-new", Ready: true},
				"blocked-old":  {PaneID: "blocked-old", Ready: true, Focused: true},
				"done-focused": {PaneID: "done-focused", Ready: true, Focused: true},
			},
		}},
	}
	state.Workspaces["remote"] = &Workspace{
		ID: "remote", Name: "service", RemoteHost: "laptop.example", Origin: Host{Name: "laptop.example"},
		Panes: map[string]*Pane{"error": {ID: "error", State: StateError, Evidence: Evidence{Event: "stop", Timestamp: base.Add(-time.Minute)}}},
	}

	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked, StateError, StateDone})
	if !snapshot.Paused || snapshot.Version != attentionSchemaVersion || snapshot.Highest != StateBlocked {
		t.Fatalf("snapshot header = %#v", snapshot)
	}
	got := make([]string, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		got = append(got, item.PaneID)
	}
	if want := []string{"blocked-old", "blocked-new", "error"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered pane IDs = %#v, want %#v", got, want)
	}
	if snapshot.Counts != (AttentionCounts{Total: 3, Blocked: 2, Error: 1}) {
		t.Fatalf("counts = %#v", snapshot.Counts)
	}
	if !snapshot.Items[1].Attached || snapshot.Items[1].Focused {
		t.Fatalf("local view flags = %#v", snapshot.Items[1])
	}
	if !snapshot.Items[0].Focused {
		t.Fatalf("focused blocked pane was not retained: %#v", snapshot.Items[0])
	}
	if got, want := snapshot.Items[2].WorkspaceRef(), "laptop.example:remote"; got != want {
		t.Fatalf("remote ref = %q, want %q", got, want)
	}
}

func TestAttentionPausePersistsAcrossDaemonRestart(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.setAttentionPaused(true); err != nil {
		t.Fatal(err)
	}
	d.Close()
	restarted, err := NewDaemon(testPaths(root), quietRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if snapshot := restarted.attentionSnapshot(); !snapshot.Paused {
		t.Fatalf("pause state was not restored: %#v", snapshot)
	}
}

func TestAttentionWatchSendsInitialSnapshotAndBroadcastsUpdates(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	api := NewAPI(d.paths)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := []chan AttentionSnapshot{make(chan AttentionSnapshot, 2), make(chan AttentionSnapshot, 2)}
	errorsCh := make(chan error, len(updates))
	for _, updatesCh := range updates {
		updatesCh := updatesCh
		go func() {
			errorsCh <- api.WatchAttention(ctx, func(snapshot AttentionSnapshot) error {
				updatesCh <- snapshot
				return nil
			})
		}()
	}
	for _, updatesCh := range updates {
		select {
		case initial := <-updatesCh:
			if initial.Counts.Total != 0 {
				t.Fatalf("initial snapshot = %#v", initial)
			}
		case <-time.After(time.Second):
			t.Fatal("initial attention snapshot timed out")
		}
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "permission_request", Source: "codex", TurnID: "turn-watch"}); err != nil {
		t.Fatal(err)
	}
	for _, updatesCh := range updates {
		select {
		case update := <-updatesCh:
			if update.Counts.Blocked != 1 || len(update.Items) != 1 || update.Items[0].PaneID != pane.ID {
				t.Fatalf("attention update = %#v", update)
			}
		case <-time.After(time.Second):
			t.Fatal("attention broadcast timed out")
		}
	}
	cancel()
	for range updates {
		if err := <-errorsCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("watch stopped with %v", err)
		}
	}
}

func TestAttentionModeAPIIsPersistentAndIdempotent(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	api := NewAPI(d.paths)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	paused, err := api.PauseAttention(ctx)
	if err != nil || !paused.Paused {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	pausedAgain, err := api.PauseAttention(ctx)
	if err != nil || !pausedAgain.Paused {
		t.Fatalf("idempotent pause = %#v, %v", pausedAgain, err)
	}
	active, err := api.ToggleAttention(ctx)
	if err != nil || active.Paused {
		t.Fatalf("toggle = %#v, %v", active, err)
	}
	active, err = api.ResumeAttention(ctx)
	if err != nil || active.Paused {
		t.Fatalf("idempotent resume = %#v, %v", active, err)
	}
}

func TestNextAttentionItemPrefersAnUnfocusedPane(t *testing.T) {
	snapshot := AttentionSnapshot{Items: []AttentionItem{
		{PaneID: "focused", Focused: true},
		{PaneID: "unfocused"},
	}}
	item, ok := nextAttentionItem(snapshot)
	if !ok || item.PaneID != "unfocused" {
		t.Fatalf("next item = %#v, %v", item, ok)
	}
}
