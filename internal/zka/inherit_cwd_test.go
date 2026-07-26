package zka

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// sourcePaneWithLiveShell marks an existing pane as hosting a live shell whose
// working directory is `live`, and returns the fake process tree describing it.
func sourcePaneWithLiveShell(t *testing.T, d *Daemon, workspaceID, paneID, live string) *fakeProcessTree {
	t.Helper()
	d.mu.Lock()
	pane := d.state.Workspaces[workspaceID].Panes[paneID]
	pane.Process.Running = true
	pane.Process.PID = 4242
	d.mu.Unlock()
	proc := &fakeProcessTree{
		cmdline:  map[int][]string{4242: paneHostArgv(workspaceID, paneID)},
		children: map[int][]int{4242: {4243}},
		cwd:      map[int]string{4243: live},
	}
	d.proc = proc
	return proc
}

// Opening a new tab from a pane whose shell has moved must land in the shell's
// current directory, not the directory that pane was created in.
func TestAllocatePaneInheritsLiveCWDFromSourcePane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	source := firstPane(workspace)
	live := t.TempDir()
	sourcePaneWithLiveShell(t, d, workspace.ID, source.ID, live)

	allocated, err := d.allocatePane(allocatePaneRequest{
		Workspace: workspace.ID, Key: "new", InheritFromPane: source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocated.Pane.CWD != live {
		t.Fatalf("new pane cwd = %q, want the source shell's live directory %q", allocated.Pane.CWD, live)
	}
}

// Without a usable live directory the caller's own cwd must survive untouched,
// which is what keeps every pre-existing caller behaving as before.
func TestAllocatePaneKeepsRequestedCWDWhenInheritanceFails(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	d.proc = &fakeProcessTree{broken: true}

	allocated, err := d.allocatePane(allocatePaneRequest{
		Workspace: workspace.ID, Key: "new",
		CWD: "/work/project", InheritFromPane: firstPane(workspace).ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocated.Pane.CWD != "/work/project" {
		t.Fatalf("new pane cwd = %q, want the requested directory", allocated.Pane.CWD)
	}
}

// A replica caches the origin's panes, but their pids name processes in the
// origin's table. Reading /proc for them locally would at best miss and at
// worst point at an unrelated local process.
func TestInheritedCWDIsNotResolvedOnAReplica(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	source := firstPane(workspace)
	proc := sourcePaneWithLiveShell(t, d, workspace.ID, source.ID, t.TempDir())
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].RemoteHost = "origin.example"
	d.mu.Unlock()

	if got := d.inheritedCWD(workspace.ID, source.ID, "/requested"); got != "/requested" {
		t.Fatalf("replica resolved a cwd: %q", got)
	}
	if proc.cwdCalls != 0 {
		t.Fatal("replica read the local process table for an origin pane")
	}
}

// Resolution stats directories and reads /proc. Doing that under the global
// mutex is the shape that caused a previous update storm, so it is pinned.
func TestInheritedCWDResolvesOutsideTheDaemonLock(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	source := firstPane(workspace)
	proc := sourcePaneWithLiveShell(t, d, workspace.ID, source.ID, t.TempDir())
	locked := true
	proc.onCWD = func() {
		if !d.mu.TryLock() {
			return
		}
		d.mu.Unlock()
		locked = false
	}
	d.inheritedCWD(workspace.ID, source.ID, "")
	if proc.cwdCalls == 0 {
		t.Fatal("resolution never reached the process tree")
	}
	if locked {
		t.Fatal("the daemon mutex was held across a filesystem syscall")
	}
}

func TestPreparePaneInheritsLiveCWDForANewPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	source := firstPane(workspace)
	live := t.TempDir()
	sourcePaneWithLiveShell(t, d, workspace.ID, source.ID, live)

	prepared, err := d.preparePane(workspacePaneRequest{
		Workspace: workspace.ID, CWD: "/requested", InheritFromPane: source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Pane.CWD != live {
		t.Fatalf("new pane cwd = %q, want %q", prepared.Pane.CWD, live)
	}
}

// An existing pane already has a directory. Preparing it again -- which happens
// on every restore -- must not move it.
func TestPreparePaneIgnoresInheritHintForAnExistingPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].Panes[panes[1].ID].CWD = "/original"
	d.mu.Unlock()
	sourcePaneWithLiveShell(t, d, workspace.ID, panes[0].ID, t.TempDir())

	prepared, err := d.preparePane(workspacePaneRequest{
		Workspace: workspace.ID, Pane: panes[1].ID, InheritFromPane: panes[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Pane.CWD != "/original" {
		t.Fatalf("existing pane cwd = %q, want it untouched", prepared.Pane.CWD)
	}
}

// The replica's own directory is a path on the wrong machine. Sending it was
// how remote panes ended up in arbitrary directories on the origin.
func TestRemotePaneAllocationNeverSendsALocalCWD(t *testing.T) {
	request := newRemotePaneAllocation("workspace", "attachment", "alloc", "source-pane")
	if request.CWD != "" {
		t.Fatalf("remote allocation carried a local cwd: %q", request.CWD)
	}
	if request.InheritFromPane != "source-pane" {
		t.Fatalf("inherit hint = %q, want the source pane", request.InheritFromPane)
	}
	if request.Key != "attachment:alloc" {
		t.Fatalf("allocation key = %q", request.Key)
	}
}

// Kitty leaves the placeholder literal when there is no active window. A
// numeric flag would make parsing fail and kill the pane.
func TestSourceWindowIDIgnoresUnparsableValues(t *testing.T) {
	for _, value := range []string{"", "@active-kitty-window-id", "0", "-3", "abc", "  "} {
		if got := sourceWindowID(value); got != 0 {
			t.Fatalf("sourceWindowID(%q) = %d, want 0", value, got)
		}
	}
	if got := sourceWindowID(" 17 "); got != 17 {
		t.Fatalf("sourceWindowID = %d, want 17", got)
	}
}

func TestPaneForWindowReadsTheZkaPaneUserVar(t *testing.T) {
	tree := []kittyOSWindow{{ID: 1, Tabs: []kittyTab{{ID: 2, Windows: []kittyWindow{
		{ID: 11, UserVars: map[string]string{"zka_workspace": "ws", "zka_pane": "pane1"}},
		{ID: 12, UserVars: map[string]string{}},
		{ID: 13, UserVars: map[string]string{"zka_workspace": "other", "zka_pane": "pane9"}},
	}}}}}
	client := KittyClient{Command: "kitten-test", Runner: &fakeRunner{
		handler: func(_ context.Context, _ string, _ ...string) (string, string, error) {
			return mustJSON(t, tree), "", nil
		},
	}}
	cases := map[int64]string{11: "pane1", 12: "", 13: "", 99: ""}
	for windowID, want := range cases {
		if got := client.PaneForWindow(context.Background(), "unix:/kitty", "ws", windowID); got != want {
			t.Fatalf("PaneForWindow(%d) = %q, want %q", windowID, got, want)
		}
	}
	if got := client.PaneForWindow(context.Background(), "", "ws", 11); got != "" {
		t.Fatalf("PaneForWindow with no endpoint = %q", got)
	}
}

func TestPaneForWindowDegradesWhenKittyFails(t *testing.T) {
	client := KittyClient{Command: "kitten-test", Runner: &fakeRunner{
		handler: func(_ context.Context, _ string, _ ...string) (string, string, error) {
			return "", "boom", context.DeadlineExceeded
		},
	}}
	if got := client.PaneForWindow(context.Background(), "unix:/kitty", "ws", 11); got != "" {
		t.Fatalf("PaneForWindow = %q, want empty on failure", got)
	}
}

// The placeholder only substitutes inside an explicit launch command, so the
// aliases must spell the managed command out. shell= must stay bare: it is the
// fallback for plain new_tab, where there is nothing to substitute.
func TestManagedKittyAliasesCarryTheSourceWindowPlaceholder(t *testing.T) {
	overrides := managedKittyOverrides("zka pane --workspace ws")
	aliases := map[string]bool{}
	shellOverride := ""
	for index, value := range overrides {
		if index == 0 || overrides[index-1] != "--override" {
			continue
		}
		if strings.HasPrefix(value, "action_alias ") {
			aliases[value] = true
		}
		if strings.HasPrefix(value, "shell=") {
			shellOverride = value
		}
	}
	if len(aliases) != 3 {
		t.Fatalf("expected three *_with_cwd aliases, got %#v", aliases)
	}
	for alias := range aliases {
		if !strings.Contains(alias, "--source-window @active-kitty-window-id") {
			t.Fatalf("alias does not identify the source window: %q", alias)
		}
		if !strings.Contains(alias, "zka pane --workspace ws") {
			t.Fatalf("alias does not spell out the managed command: %q", alias)
		}
	}
	if shellOverride != "shell=zka pane --workspace ws" {
		t.Fatalf("shell override = %q, want the bare managed command", shellOverride)
	}
}

// The origin dispatcher used to drop every field it did not name explicitly.
// The inherit hint is the only cwd signal a remote pane has, so losing it here
// would silently put every remote pane in the wrong directory.
func TestRemoteAllocatePaneCarriesTheInheritHint(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	source := firstPane(workspace)
	live := t.TempDir()
	sourcePaneWithLiveShell(t, d, workspace.ID, source.ID, live)

	payload, _ := json.Marshal(allocatePaneRequest{
		Workspace: workspace.ID, Key: "attachment:alloc", InheritFromPane: source.ID,
		// Attachment-local Kitty identity from the *requesting* host. It must
		// not reach the origin, which would otherwise chase a Kitty that lives
		// on another machine.
		Endpoint: "unix:/replica-kitty", WindowID: 77,
	})
	raw, err := dispatchRemoteControl(context.Background(), NewAPI(d.paths), "allocate_pane", payload)
	if err != nil {
		t.Fatal(err)
	}
	var allocated allocatePaneResponse
	if err := json.Unmarshal(raw, &allocated); err != nil {
		t.Fatal(err)
	}
	if allocated.Pane.CWD != live {
		t.Fatalf("remote pane cwd = %q, want the source shell's directory %q", allocated.Pane.CWD, live)
	}
	if allocated.Pane.Admission.Endpoint != "" || allocated.Pane.Admission.WindowID != 0 {
		t.Fatalf("replica Kitty identity crossed the host boundary: %#v", allocated.Pane.Admission)
	}
}
