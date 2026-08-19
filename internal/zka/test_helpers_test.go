package zka

import (
	"bytes"
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

func (f *fakeRunner) RunConfigured(ctx context.Context, name string, args []string, _ commandOptions) (string, string, error) {
	return f.Run(ctx, name, args...)
}

var _ configuredCommandRunner = (*fakeRunner)(nil)

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

// fakeNotifier replaces the D-Bus desktop notifier in tests. D-Bus has no argv,
// so fakeRunner cannot intercept it; without this a test machine that happens to
// have a session bus would post real notifications to the developer's screen.
type fakeNotifier struct {
	mu        sync.Mutex
	notes     []DesktopNotification
	withdrawn []paneRef
	probes    int
	err       error
}

func (f *fakeNotifier) Notify(_ context.Context, note DesktopNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, note)
	return f.err
}

func (f *fakeNotifier) Withdraw(_ context.Context, workspaceID, paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawn = append(f.withdrawn, paneRef{Workspace: workspaceID, Pane: paneID})
}

func (f *fakeNotifier) Probe(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return "fake notification server", f.err
}

func (f *fakeNotifier) Shutdown() {}

func (f *fakeNotifier) Notes() []DesktopNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DesktopNotification(nil), f.notes...)
}

func (f *fakeNotifier) Withdrawn() []paneRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]paneRef(nil), f.withdrawn...)
}

func (f *fakeNotifier) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeDesktop returns the notifier installed by the test daemon constructors.
func fakeDesktop(t testing.TB, d *Daemon) *fakeNotifier {
	t.Helper()
	notifier, ok := d.desktop.(*fakeNotifier)
	if !ok {
		t.Fatalf("desktop notifier is %T, want *fakeNotifier", d.desktop)
	}
	return notifier
}

// testSocketReserve is the longest suffix the daemon appends to a test root
// before binding: RuntimeDir, the hook relay root, a 32-character session id
// and ".sock". Every other runtime socket is shorter than this one.
const testSocketReserve = len("/run/hook-relays/") + 32 + len(".sock")

// testRoot returns a scratch directory short enough that every Unix socket the
// daemon binds underneath it stays inside the sockaddr_un ceiling.
//
// t.TempDir bakes the test name into the path, so a descriptive test name under
// an ordinary TMPDIR already overflows before the test body runs: under Nix's
// /build the daemon and hook relay sockets reached 105 and 128 bytes against a
// 103-byte limit. Any test that binds a socket needs this instead of t.TempDir.
func testRoot(t testing.TB) string {
	t.Helper()
	bases := []string{""}
	if os.TempDir() != "/tmp" {
		bases = append(bases, "/tmp")
	}
	var rejected string
	for _, base := range bases {
		root, err := os.MkdirTemp(base, "zka-t-")
		if err != nil {
			t.Fatal(err)
		}
		if len(root)+testSocketReserve <= safeUnixSocketPath {
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			return root
		}
		_ = os.RemoveAll(root)
		rejected = root
	}
	t.Fatalf("no temporary directory is short enough for a Unix socket: %q leaves %d bytes for a %d-byte suffix; point TMPDIR at a shorter path",
		rejected, safeUnixSocketPath-len(rejected), testSocketReserve)
	return ""
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
	t.Setenv("ZKA_HOOK_SOCKET", "")
	d, err := NewDaemon(testPaths(root), runner, log.New(io.Discard, "", 0))
	if err == nil {
		d.desktop = &fakeNotifier{}
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, err
}

// syncBuffer captures the daemon journal. log.Logger is written from several
// workers at once, so the buffer needs its own lock.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestDaemonWithLog is newTestDaemon plus a readable journal. A delivery
// failure is observable only through the logger, so a test that cannot read it
// cannot prove the failure was reported.
func newTestDaemonWithLog(t testing.TB, root string, runner CommandRunner) (*Daemon, *syncBuffer, error) {
	t.Helper()
	t.Setenv("ZKA_CONFIG", "")
	t.Setenv("ZKA_HOOK_SOCKET", "")
	journal := &syncBuffer{}
	d, err := NewDaemon(testPaths(root), runner, log.New(journal, "", 0))
	if err == nil {
		d.desktop = &fakeNotifier{}
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, journal, err
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

func readyCredentialTransport(t testing.TB, d *Daemon, provider Host) string {
	t.Helper()
	path := filepath.Join(d.paths.RuntimeDir, "test-credential-transport-"+shortID(provider.ID)+".sock")
	listener, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: provider, State: "ready", Endpoint: path}); err != nil {
		t.Fatal(err)
	}
	return path
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

func addReadyCredentialAttachment(d *Daemon, workspaceID string, node Host) string {
	id := "credential-owner-" + node.ID
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace.Attachments == nil {
		workspace.Attachments = map[string]*Attachment{}
	}
	workspace.Attachments[id] = &Attachment{
		ID: id, Node: node, Status: AttachmentReady, Transport: Transport{Kind: "ssh"},
		Endpoint: "ssh:" + node.Name + ":" + id, Views: map[string]RuntimeView{}, ClientHeartbeats: map[string]time.Time{},
	}
	return id
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

// kittyTreeForTabs builds a realistic single-OS-window Kitty tree. See
// kittyTreeForOSWindows for the general form.
func kittyTreeForTabs(workspaceID string, tabs [][]string) []kittyOSWindow {
	return kittyTreeForOSWindows(workspaceID, [][][]string{tabs})
}

// kittyTreeForOSWindows builds a realistic Kitty tree from
// OS-window -> tab -> pane ids. In particular, every tab carries the
// enabled_layouts and layout_state fields a real Kitty always reports.
// Fixtures that omit those fields hide exactly the class of structural
// publication bug this package exists to prevent.
func kittyTreeForOSWindows(workspaceID string, roots [][][]string) []kittyOSWindow {
	// Tabs are unnamed unless something explicitly named them, which is what
	// Kitty reports via title_overridden. An unnamed tab echoes its active
	// window's title, and capturing that as a desired name is precisely the
	// mistake that pins transient program titles into the topology.
	unnamed := false
	windowID := int64(10)
	tabID := int64(100)
	var tree []kittyOSWindow
	for rootIndex, tabs := range roots {
		osWindow := kittyOSWindow{ID: int64(rootIndex + 1), WMClass: "managed-kitty"}
		for tabIndex, panes := range tabs {
			tab := kittyTab{
				ID: tabID, Layout: "splits", IsActive: tabIndex == 0,
				Enabled:         []string{"fat", "grid", "horizontal", "splits", "stack", "tall", "vertical"},
				TitleOverridden: &unnamed, Title: "shell",
			}
			tabID++
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
		tree = append(tree, osWindow)
	}
	return tree
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
		d.desktop = &fakeNotifier{}
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, err
}
