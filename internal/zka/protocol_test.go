package zka

import (
	"context"
	"testing"
)

func TestDaemonProtocolRoundTrip(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	api := NewAPI(d.paths)
	if _, err := api.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace, err := api.CreateWorkspace(context.Background(), createWorkspaceRequest{Name: "one", Shell: []string{"fish"}, Panes: []PaneSpec{{CWD: "/work"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.Workspace(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != workspace.ID || len(got.Panes) != 1 {
		t.Fatalf("workspace = %#v", got)
	}
	node, err := api.Node(context.Background())
	if err != nil || node.ID == "" {
		t.Fatalf("node = %#v, %v", node, err)
	}
}

func TestProtocolRejectsUnknownOperation(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	if err := (Client{Socket: d.paths.Socket}).Call(context.Background(), "nope", nil, nil); err == nil {
		t.Fatal("unknown operation succeeded")
	}
}
