package zka

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureSnapshotFiltersUnmanagedWindows(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "--version":
			return "kitten 0.47.0\n", "", nil
		case strings.Contains(joined, "--output-format=session"):
			return "layout splits\nlaunch zka view abc\n", "", nil
		default:
			return `[{
  "id": 1,
  "is_focused": true,
  "tabs": [{
    "id": 2,
    "title": "work",
    "layout": "splits",
    "is_active": true,
    "windows": [
      {"id": 3, "title": "agent", "cwd": "/work", "is_active": true, "user_vars": {"zka_session": "abc"}},
      {"id": 4, "title": "shell", "cwd": "/work", "user_vars": {}}
    ]
  }]
}]`, "", nil
		}
	}}
	snapshot, err := CaptureSnapshot(context.Background(), KittyClient{Runner: runner}, "unix:/kitty", "daily")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.KittyVersion != "kitten 0.47.0" || len(snapshot.OSWindows) != 1 || len(snapshot.OSWindows[0].Tabs[0].Views) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if snapshot.OSWindows[0].Tabs[0].Views[0].SessionID != "abc" {
		t.Fatalf("wrong managed view: %#v", snapshot.OSWindows[0].Tabs[0].Views)
	}
}

func TestGenerateKittySessionSkipsExistingViews(t *testing.T) {
	snapshot := Snapshot{Name: "daily", OSWindows: []SnapshotOSWindow{{Focused: true, Tabs: []SnapshotTab{{Title: "agents", Layout: "splits", Enabled: []string{"splits", "stack"}, LayoutState: json.RawMessage(`{"bias":40}`), Active: true, Views: []SnapshotView{{SessionID: "one", Title: "One", CWD: "/one"}, {SessionID: "two", Title: "Two", CWD: "/two", Active: true}}}}}}}
	sessions := map[string]*Session{
		"one": {ID: "one", Backend: BackendRef{Kind: "zmx"}, State: StateIdle},
		"two": {ID: "two", Backend: BackendRef{Kind: "zmx"}, State: StateDone},
	}
	got, err := GenerateKittySession(snapshot, sessions, map[string]bool{"one": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "zka_session=one") || !strings.Contains(got, "zka_session=two") || !strings.Contains(got, "focus_os_window") || !strings.Contains(got, "new_tab agents") || !strings.Contains(got, "enabled_layouts splits,stack") {
		t.Fatalf("generated session:\n%s", got)
	}
}

func TestGenerateKittySessionRejectsUnknownSession(t *testing.T) {
	snapshot := Snapshot{Name: "bad", OSWindows: []SnapshotOSWindow{{Tabs: []SnapshotTab{{Views: []SnapshotView{{SessionID: "missing"}}}}}}}
	if _, err := GenerateKittySession(snapshot, map[string]*Session{}, nil); err == nil {
		t.Fatal("unknown session accepted")
	}
}

func TestNativeSessionMustContainOnlyManagedViewCommands(t *testing.T) {
	if !nativeSessionIsManaged("layout splits\nlaunch --var zka_session=abc zka view abc\n", []string{"abc"}) {
		t.Fatal("managed native session rejected")
	}
	if nativeSessionIsManaged("launch --var zka_session=abc sh -c evil\n", []string{"abc"}) {
		t.Fatal("arbitrary native command accepted")
	}
}
