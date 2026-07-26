package zka

import (
	"context"
	"encoding/json"
	"fmt"
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
		AgentDir:      filepath.Join(runtime, "agents"),
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

// testAllocatePane and testPreparePane keep the pre-provenance call shape that
// most tests use; production callers supply Kitty window identity.
func testAllocatePane(d *Daemon, workspace, key, cwd string) (allocatePaneResponse, error) {
	return d.allocatePane(allocatePaneRequest{Workspace: workspace, Key: key, CWD: cwd})
}

func testPreparePane(d *Daemon, workspace, pane, cwd string) (preparePaneResponse, error) {
	return d.preparePane(workspacePaneRequest{Workspace: workspace, Pane: pane, CWD: cwd})
}

// kittyTreeForTabs builds a realistic Kitty tree: one OS window, one tab per
// group, with the enabled_layouts and layout_state a real Kitty always reports.
// Fixtures that omit those fields hide exactly the class of bug this package
// exists to prevent.
func kittyTreeForTabs(workspaceID string, tabs [][]string) []kittyOSWindow {
	// Tabs are unnamed unless something explicitly named them, which is what
	// Kitty reports via title_overridden. An unnamed tab echoes its active
	// window's title, and capturing that as a desired name is precisely the
	// mistake that pins transient program titles into the topology.
	unnamed := false
	osWindow := kittyOSWindow{ID: 1, WMClass: "managed-kitty"}
	windowID := int64(10)
	for index, panes := range tabs {
		tab := kittyTab{
			ID: int64(100 + index), Layout: "splits", IsActive: index == 0,
			Enabled:         []string{"fat", "grid", "horizontal", "splits", "stack", "tall", "vertical"},
			TitleOverridden: &unnamed, Title: "shell",
		}
		var groups []string
		for offset, paneID := range panes {
			windowID++
			tab.Windows = append(tab.Windows, kittyWindow{
				ID: windowID, Title: "shell", IsActive: offset == 0,
				UserVars: map[string]string{
					"zka_workspace": workspaceID, "zka_pane": paneID, "zka_ready": "1",
				},
				Cmdline: []string{"zka", "pane", "--workspace", workspaceID},
			})
			groups = append(groups, fmt.Sprintf(`{"id":%d,"window_ids":[%d]}`, windowID, windowID))
		}
		tab.LayoutState = json.RawMessage(fmt.Sprintf(
			`{"all_windows":{"active_group_idx":0,"active_group_history":[],"window_groups":[%s]},"class":"Splits","opts":{}}`,
			strings.Join(groups, ",")))
		osWindow.Tabs = append(osWindow.Tabs, tab)
	}
	return []kittyOSWindow{osWindow}
}

// kittyResponse answers the three `kitten @` calls a capture makes.
func kittyResponse(t testing.TB, args []string, tree []kittyOSWindow, workspace *Workspace) (string, string, error) {
	t.Helper()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--output-format=session"):
		var out sessionWriter
		for _, osWindow := range tree {
			for _, tab := range osWindow.Tabs {
				out.NewTab(tab.Title)
				for _, window := range tab.Windows {
					pane := workspace.Panes[window.UserVars["zka_pane"]]
					if pane == nil {
						continue
					}
					out.Launch(buildLaunch(launchSpec{
						Workspace: workspace, Pane: pane, Transport: Transport{Kind: "local"},
					}))
				}
			}
		}
		return out.String(), "", nil
	case strings.HasSuffix(joined, "ls"):
		return mustJSON(t, tree), "", nil
	case strings.Contains(joined, "--version"):
		return "kitten 0.47.0", "", nil
	}
	return "", "", nil
}

// newTestDaemonAtPaths builds a daemon over an existing state directory, so a
// test can seed persisted state before the daemon loads it.
func newTestDaemonAtPaths(t testing.TB, paths Paths, runner CommandRunner) (*Daemon, error) {
	t.Helper()
	t.Setenv("ZKA_CONFIG", "")
	d, err := NewDaemon(paths, runner, log.New(io.Discard, "", 0))
	if err == nil {
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, err
}
