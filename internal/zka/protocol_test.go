package zka

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDaemonProtocolRoundTrip(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	waitFor(t, func() bool {
		_, err := os.Stat(d.paths.Socket)
		return err == nil
	})
	api := NewAPI(d.paths)
	if _, err := api.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, err := api.CreateSession(context.Background(), createSessionRequest{Name: "one", Command: []string{"codex"}, CWD: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.Session(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != session.ID {
		t.Fatalf("session ids differ: %s != %s", got.ID, session.ID)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestProtocolRejectsUnknownOperation(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx) }()
	waitFor(t, func() bool {
		_, err := os.Stat(d.paths.Socket)
		return err == nil
	})
	client := Client{Socket: d.paths.Socket}
	if err := client.Call(context.Background(), "nope", nil, nil); err == nil {
		t.Fatal("unknown operation succeeded")
	}
}
