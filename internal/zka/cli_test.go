package zka

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaneCommandEnvironmentUsesStableAgentOnlyForNewBackends(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ephemeral-agent.sock")
	t.Setenv("ZKA_AGENT_RELAY_VERSION", "")
	paths := testPaths(t.TempDir())
	var cfg Config
	cfg.SSH.ForwardAgent = true
	created := paneCommandEnvironment(cfg, paths, "workspace", "pane", true)
	if got := testEnvironmentValue(created, "SSH_AUTH_SOCK"); got != agentRelaySocketPath(paths.AgentDir, "workspace") {
		t.Fatalf("created SSH_AUTH_SOCK = %q", got)
	}
	if got := testEnvironmentValue(created, "ZKA_AGENT_RELAY_VERSION"); got != "1" {
		t.Fatalf("relay version = %q", got)
	}
	attached := paneCommandEnvironment(cfg, paths, "workspace", "pane", false)
	if got := testEnvironmentValue(attached, "SSH_AUTH_SOCK"); got != "/tmp/ephemeral-agent.sock" {
		t.Fatalf("reattach SSH_AUTH_SOCK = %q", got)
	}
}

func testEnvironmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestWorkspaceAgentStatusCLIReportsDisabledByDefault(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	serveTestDaemon(t, d)
	var output bytes.Buffer
	code, err := runWorkspaceAgent([]string{"status", "--json", workspace.ID}, d.paths, &output, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("status code = %d, err = %v", code, err)
	}
	if !strings.Contains(output.String(), `"state": "disabled"`) || !strings.Contains(output.String(), `"owner": "origin"`) {
		t.Fatalf("status = %s", output.String())
	}
}

func TestInterspersedWorkspaceFlagsMatchDocumentedSyntax(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pane := fs.String("pane", "", "")
	jsonOut := fs.Bool("json", false, "")
	if err := parseInterspersed(fs, []string{"devbox.example:example-project", "--pane", "abc", "--json"}); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "devbox.example:example-project" || *pane != "abc" || !*jsonOut {
		t.Fatalf("args=%#v pane=%q json=%v", fs.Args(), *pane, *jsonOut)
	}
}

func TestWorkspaceAttachIsTheOnlyAttachCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runWorkspace([]string{"attach"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || err.Error() != "workspace attach requires one workspace reference" {
		t.Fatalf("attach: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code, err = runWorkspace([]string{"open"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || err.Error() != `unknown workspace command "open"` {
		t.Fatalf("open: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "\n  open ") || !strings.Contains(stderr.String(), "\n  attach ") {
		t.Fatalf("workspace usage did not advertise attach exclusively: %q", stderr.String())
	}
}

func TestLaunchIsAStandaloneTopLevelCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run([]string{"launch", "unexpected"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || err == nil || err.Error() != "launch accepts no arguments" {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	printUsage(&stdout)
	if !strings.Contains(stdout.String(), "launch      Choose or create a workspace") {
		t.Fatalf("top-level usage does not advertise the launcher: %q", stdout.String())
	}
}

func TestLauncherProxyReturnsHelperExitStatus(t *testing.T) {
	for _, test := range []struct {
		command string
		want    int
	}{
		{command: "true", want: 0},
		{command: "false", want: 1},
	} {
		t.Run(test.command, func(t *testing.T) {
			t.Setenv("ZKA_LAUNCHER_COMMAND", test.command)
			code, err := runLauncher(nil, strings.NewReader(""), io.Discard, io.Discard)
			if err != nil || code != test.want {
				t.Fatalf("code=%d err=%v, want code %d", code, err, test.want)
			}
		})
	}
}

func TestKittyPassthroughRejectsManagedProcessOptions(t *testing.T) {
	for _, args := range [][]string{{"--listen-on", "unix:/other"}, {"--session=x"}, {"--detach"}, {"--override", "shell=bash"}, {"bash"}} {
		if err := validateKittyPassthrough(args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
	if err := validateKittyPassthrough([]string{"--class", "managed", "--override", "font_size=12"}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedKittyPreservesLastReportedCWDForNewPanes(t *testing.T) {
	joined := strings.Join(managedKittyOverrides("zka pane --workspace workspace"), "\n")
	for _, expected := range []string{
		"action_alias new_tab_with_cwd launch --type=tab --cwd=last_reported",
		"action_alias new_window_with_cwd launch --type=window --cwd=last_reported",
		"action_alias new_os_window_with_cwd launch --type=os-window --cwd=last_reported",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("managed Kitty overrides missing %q: %s", expected, joined)
		}
	}
}

func TestDeadPaneMessageWaitsForCtrlC(t *testing.T) {
	workspace := &Workspace{ID: "workspace", Name: "main"}
	pane := &Pane{ID: "pane-12345678", Backend: BackendRef{Ref: "zka-workspace-pane"}}
	var output bytes.Buffer
	if err := writeDeadPaneMessage(&output, workspace, pane, errors.New("backend crashed\nnow")); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"zmx backend is dead", "workspace: main", "zka-workspace-pane", "Press Ctrl-C to remove this pane", "backend crashed now"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("dead pane output missing %q: %q", expected, output.String())
		}
	}
	if err := waitForDeadPaneDismiss(bytes.NewReader([]byte("ignored\x03"))); err != nil {
		t.Fatal(err)
	}
}

func TestFinishLocalPaneAttachDoesNotTombstoneCleanExit(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	api := NewAPI(d.paths)
	for index, pane := range panes {
		if _, err := api.Event(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "pane-host", PID: 40 + index,
		}); err != nil {
			t.Fatal(err)
		}
	}
	code := 0
	if _, err := api.Event(context.Background(), Event{
		WorkspaceID: workspace.ID, PaneID: panes[0].ID, Kind: "process_exit", Source: "pane-host",
		ExitCode: &code, Detail: "exit code 0",
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	gotCode, err := finishLocalPaneAttach(
		api,
		Config{},
		KittyClient{Runner: &fakeRunner{}, Command: "kitten-test"},
		"unix:/kitty",
		42,
		workspace,
		panes[0],
		nil,
		bytes.NewReader([]byte{3}),
		&output,
	)
	if err != nil || gotCode != 0 {
		t.Fatalf("code=%d err=%v", gotCode, err)
	}
	if output.Len() != 0 {
		t.Fatalf("clean exit rendered a tombstone: %q", output.String())
	}
	fresh, err := api.Workspace(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.Panes[panes[0].ID].Evidence.Event; got != "process_exit" {
		t.Fatalf("clean exit was reclassified as %q", got)
	}
}

func TestAttachmentIDIsStablePerNodeWorkspace(t *testing.T) {
	a := localAttachmentID("node", "workspace")
	if a != localAttachmentID("node", "workspace") || a == localAttachmentID("other", "workspace") {
		t.Fatalf("attachment ids are not deterministic: %q", a)
	}
}

func TestRunKittyReportsPrelaunchFailureWithoutPanic(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	t.Setenv("ZKA_KITTY_WATCHER", filepath.Join(t.TempDir(), "missing-watcher.py"))

	var stdout, stderr bytes.Buffer
	code, err := runKitty(nil, d.paths, &stdout, &stderr)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Kitty watcher not found") {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if workspaces := d.listWorkspaces(); len(workspaces) != 0 {
		t.Fatalf("failed prelaunch retained workspaces: %#v", workspaces)
	}
}

func TestPreferredLocalAttachmentReusesReadyAlternateInstance(t *testing.T) {
	workspace := &Workspace{
		ID: "workspace", PrimaryAttachmentID: "stale",
		Attachments: map[string]*Attachment{
			"stale": {ID: "stale", Node: Host{ID: "node"}, Endpoint: "unix:/stale", Role: AttachmentPrimary, Status: AttachmentUnhealthy},
			"ready": {ID: "ready", Node: Host{ID: "node"}, Endpoint: "unix:/ready", Role: AttachmentMirror, Status: AttachmentReady},
		},
	}
	if got := preferredLocalAttachment(workspace, "node"); got == nil || got.ID != "ready" {
		t.Fatalf("preferred attachment = %#v", got)
	}
}

func TestFocusableLocalAttachmentKeepsUnhealthyAttachedPaneInExistingKitty(t *testing.T) {
	workspace := &Workspace{
		ID: "workspace", PrimaryAttachmentID: "existing",
		Attachments: map[string]*Attachment{
			"existing": {
				ID: "existing", Node: Host{ID: "node"}, Endpoint: "unix:/existing", Status: AttachmentUnhealthy,
				Views: map[string]RuntimeView{"pane": {PaneID: "pane", WindowID: 9, Ready: true}},
			},
		},
	}
	if got := focusableLocalAttachment(workspace, "node", "pane"); got == nil || got.ID != "existing" {
		t.Fatalf("focusable attachment = %#v", got)
	}
}

func TestWorkspaceAttachRefusesRequestedPaneAfterZMXReconcileMarksItDead(t *testing.T) {
	runner := newLifecycleRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	for _, pane := range panes {
		if _, err := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner.setSession(panes[0].Backend.Ref, true)
	workspace, existing := readyWorkspaceAttachment(t, d, workspace, "existing")
	d.markAttachmentUnhealthy(workspace.ID, existing.ID, errors.New("stale capture after pane exit"))
	serveTestDaemon(t, d)

	var stdout, stderr bytes.Buffer
	code, err := runWorkspaceAttach([]string{workspace.ID, "--pane", panes[1].ID}, d.paths, false, &stdout, &stderr)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "zmx backend is dead") {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	fresh, getErr := NewAPI(d.paths).Workspace(context.Background(), workspace.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(fresh.Attachments) != 1 || fresh.Attachments[existing.ID] == nil {
		t.Fatalf("dead pane attach duplicated the existing Kitty attachment: %#v", fresh.Attachments)
	}
}

func TestWorkspaceRenameAndKillCLI(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)

	var stdout, stderr bytes.Buffer
	code, err := runWorkspaceRename([]string{workspace.ID, "shell-work"}, d.paths, &stdout, &stderr)
	if err != nil || code != 0 || !strings.Contains(stdout.String(), workspace.ID+"\tshell-work") {
		t.Fatalf("rename: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	renamed, err := NewAPI(d.paths).Workspace(context.Background(), workspace.ID)
	if err != nil || renamed.Name != "shell-work" {
		t.Fatalf("renamed workspace = %#v, %v", renamed, err)
	}

	stdout.Reset()
	stderr.Reset()
	code, err = runWorkspaceKill([]string{workspace.ID}, d.paths, &stdout, &stderr)
	if err != nil || code != 0 || !strings.Contains(stdout.String(), workspace.ID+"\tshell-work") {
		t.Fatalf("kill: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if _, err := NewAPI(d.paths).Workspace(context.Background(), workspace.ID); err == nil {
		t.Fatal("killed workspace remained visible")
	}
}
