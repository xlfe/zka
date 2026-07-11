package zka

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"sync"
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

func testPaths(root string) Paths {
	state := filepath.Join(root, "state")
	runtime := filepath.Join(root, "run")
	return Paths{
		StateDir:    state,
		RuntimeDir:  runtime,
		StateFile:   filepath.Join(state, "state.json"),
		SnapshotDir: filepath.Join(state, "snapshots"),
		Socket:      filepath.Join(runtime, "zka.sock"),
	}
}

func newTestDaemon(root string, runner CommandRunner) (*Daemon, error) {
	return NewDaemon(testPaths(root), runner, log.New(io.Discard, "", 0))
}
