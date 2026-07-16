package zka

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type runnerCall struct {
	Name string
	Args []string
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []runnerCall
	handler func(context.Context, string, ...string) (string, string, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, runnerCall{Name: name, Args: append([]string(nil), args...)})
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(ctx, name, args...)
	}
	return "", "", nil
}

func (f *fakeRunner) Calls() []runnerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runnerCall(nil), f.calls...)
}

func quietRunner() *fakeRunner {
	return &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		if name == "kitten" && strings.Contains(joined, " ls") {
			return "[]", "", nil
		}
		if name == "kitten" && joined == "--version" {
			return "kitten 0.47.4\n", "", nil
		}
		return "", "", nil
	}}
}

func testPaths(root string) Paths {
	state := filepath.Join(root, "state")
	runtime := filepath.Join(root, "run")
	return Paths{
		StateDir: state, RuntimeDir: runtime,
		StateFile:     filepath.Join(state, "state.json"),
		GeneratedDir:  filepath.Join(state, "generated"),
		AttachmentDir: filepath.Join(runtime, "kitty"),
		Socket:        filepath.Join(runtime, "zka.sock"),
		WatcherSocket: filepath.Join(runtime, "watcher.sock"),
	}
}

func newTestDaemon(t testing.TB, root string, runner CommandRunner) (*Daemon, error) {
	t.Helper()
	t.Setenv("ZKA_CONFIG", "")
	d, err := NewDaemon(testPaths(root), runner, log.New(io.Discard, "", 0))
	if err == nil {
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, err
}

func serveTestDaemon(t testing.TB, d *Daemon) {
	t.Helper()
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("stop test daemon: %v", err)
		}
		if err := d.Wait(); err != nil {
			t.Errorf("wait for test daemon: %v", err)
		}
	})
	waitFor(t, func() bool { _, err := os.Stat(d.paths.Socket); return err == nil })
}

func createTestWorkspace(t testing.TB, daemon *Daemon, panes int) *Workspace {
	t.Helper()
	specs := make([]PaneSpec, panes)
	for i := range specs {
		specs[i] = PaneSpec{CWD: "/work", Title: "pane"}
	}
	workspace, err := daemon.createWorkspace(createWorkspaceRequest{Name: "reviewer", Shell: []string{"fish"}, Panes: specs})
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func waitFor(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}

func hasCommand(calls []runnerCall, name string) bool {
	for _, call := range calls {
		if call.Name == name {
			return true
		}
	}
	return false
}

func firstCommand(calls []runnerCall, name string) runnerCall {
	for _, call := range calls {
		if call.Name == name {
			return call
		}
	}
	return runnerCall{}
}
