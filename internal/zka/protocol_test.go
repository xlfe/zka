package zka

import (
	"context"
	"errors"
	"testing"
)

func TestDaemonProtocolRoundTrip(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	err = (Client{Socket: d.paths.Socket}).Call(context.Background(), "nope", nil, nil)
	if err == nil {
		t.Fatal("unknown operation succeeded")
	}
	if got, want := err.Error(), `unknown operation "nope"`; got != want {
		t.Fatalf("unknown operation error = %q, want %q", got, want)
	}
	if !isUnknownDaemonOperation(err, "nope") || isUnknownDaemonOperation(errors.New("unsupported operation nope"), "nope") {
		t.Fatalf("unknown-operation compatibility match accepted the wrong error: %v", err)
	}
}
