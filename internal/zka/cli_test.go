package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalAndRemotePaneBackendCommandsUseStableCredentialEnvironment(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/local-agent.sock")
	t.Setenv("GNUPGHOME", "/tmp/local-gnupg")
	t.Setenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "2")
	paths := testPaths(testRoot(t))
	var cfg Config
	cfg.ZMX.Command = "zmx"
	var sshBundle, openPGPBundle CredentialBundleConfig
	sshBundle.SSHAgent.Enable = true
	openPGPBundle.OpenPGP.Enable = true
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{"ssh": sshBundle, "openpgp": openPGPBundle}
	prepared := preparePaneResponse{
		Workspace: &Workspace{ID: "workspace", Shell: []string{"fish"}},
		Pane:      &Pane{ID: "pane", Backend: BackendRef{Ref: "backend"}},
		Create:    true,
	}

	local := localPaneBackendCommand(cfg, paths, prepared)
	if got := testEnvironmentValue(local.Env, "SSH_AUTH_SOCK"); got != agentRelaySocketPath(paths.AgentDir, "workspace") {
		t.Fatalf("local SSH_AUTH_SOCK = %q", got)
	}
	if got, want := testEnvironmentValue(local.Env, "GNUPGHOME"), filepath.Join(paths.StateDir, "credentials", "workspace", "gnupg"); got != want {
		t.Fatalf("local GNUPGHOME = %q, want %q", got, want)
	}
	if got := testEnvironmentValue(local.Env, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"); got != "4" {
		t.Fatalf("local credential environment version = %q", got)
	}

	remote := remotePaneBackendCommand(cfg, paths, prepared)
	if got := testEnvironmentValue(remote.Env, "SSH_AUTH_SOCK"); got != agentRelaySocketPath(paths.AgentDir, "workspace") {
		t.Fatalf("remote SSH_AUTH_SOCK = %q", got)
	}
	if got, want := testEnvironmentValue(remote.Env, "GNUPGHOME"), filepath.Join(paths.StateDir, "credentials", "workspace", "gnupg"); got != want {
		t.Fatalf("remote GNUPGHOME = %q, want %q", got, want)
	}
	if got := testEnvironmentValue(remote.Env, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"); got != "4" {
		t.Fatalf("remote credential environment version = %q", got)
	}

	prepared.Create = false
	attached := remotePaneBackendCommand(cfg, paths, prepared)
	if got := testEnvironmentValue(attached.Env, "SSH_AUTH_SOCK"); got != "/tmp/local-agent.sock" {
		t.Fatalf("reattach process SSH_AUTH_SOCK = %q", got)
	}
	if testEnvironmentContains(attached.Env, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION") {
		t.Fatalf("reattach process claimed to rewrite the backend environment: %#v", attached.Env)
	}
}

func TestLocalPaneBackendCommandPreservesUnsetCredentialEnvironment(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "restore-after-test")
	t.Setenv("GNUPGHOME", "restore-after-test")
	if err := os.Unsetenv("SSH_AUTH_SOCK"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GNUPGHOME"); err != nil {
		t.Fatal(err)
	}
	prepared := preparePaneResponse{
		Workspace: &Workspace{ID: "workspace", Shell: []string{"fish"}},
		Pane:      &Pane{ID: "pane", Backend: BackendRef{Ref: "backend"}},
		Create:    true,
	}
	var cfg Config
	cfg.ZMX.Command = "zmx"
	cmd := localPaneBackendCommand(cfg, Paths{}, prepared)
	for _, name := range []string{"SSH_AUTH_SOCK", "GNUPGHOME", "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"} {
		if testEnvironmentContains(cmd.Env, name) {
			t.Fatalf("local command invented %s: %#v", name, cmd.Env)
		}
	}
}

func TestManagedPaneEnvironmentRemovesUnconfiguredAmbientCredentials(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ambient-agent.sock")
	t.Setenv("GNUPGHOME", "/tmp/ambient-gnupg")
	var cfg Config
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{"pivb": bundle}
	environment := managedPaneCommandEnvironment(cfg, testPaths(testRoot(t)), "workspace", "pane", true)
	for _, name := range []string{"SSH_AUTH_SOCK", "GNUPGHOME"} {
		if testEnvironmentContains(environment, name) {
			t.Fatalf("managed environment leaked ambient %s: %#v", name, environment)
		}
	}
	if got := testEnvironmentValue(environment, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"); got != "4" {
		t.Fatalf("managed credential environment version = %q", got)
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

func testEnvironmentContains(environment []string, name string) bool {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func TestWorkspaceCredentialsStatusCLIReportsUnclaimedByDefault(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	serveTestDaemon(t, d)
	var output bytes.Buffer
	code, err := runWorkspaceCredentials([]string{"status", "--json", workspace.ID}, d.paths, &output, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("status code = %d, err = %v", code, err)
	}
	if !strings.Contains(output.String(), `"state": "unclaimed"`) || !strings.Contains(output.String(), `"capabilities": {}`) {
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

func TestWorkspaceCreateDispatchAndUsage(t *testing.T) {
	// CLI validation must not inherit the developer's configured default
	// credential bundle. These cases intentionally stop before any RPC.
	t.Setenv("ZKA_CONFIG", "")
	var stdout, stderr bytes.Buffer
	code, err := runWorkspace([]string{"create"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "requires one [SSH_ALIAS:]NAME") {
		t.Fatalf("no argument: code=%d err=%v", code, err)
	}
	code, err = runWorkspace([]string{"create", ""}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "requires a name") {
		t.Fatalf("empty local name: code=%d err=%v", code, err)
	}
	code, err = runWorkspace([]string{"create", "one", "two"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil {
		t.Fatalf("two arguments: code=%d err=%v", code, err)
	}
	// A relative --cwd for a remote origin is rejected before any RPC: it
	// would silently mean a different directory over there.
	code, err = runWorkspace([]string{"create", "devbox.example:api", "--cwd", "relative"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "absolute path on devbox.example") {
		t.Fatalf("relative remote cwd: code=%d err=%v", code, err)
	}
	code, err = runWorkspace([]string{"create", "devbox.example:api", "--claim-credentials"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "credentials.default_bundle is not set") {
		t.Fatalf("claim without attach: code=%d err=%v", code, err)
	}
	code, err = runWorkspace([]string{"create", "api", "--attach", "--claim-credentials"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "credentials.default_bundle is not set") {
		t.Fatalf("local create claim: code=%d err=%v", code, err)
	}
	code, err = runWorkspace([]string{"attach", "api", "--claim-credentials"}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--claim-credentials requires a remote workspace") {
		t.Fatalf("local attach claim: code=%d err=%v", code, err)
	}
	stdout.Reset()
	if code, err := runWorkspace([]string{"help"}, Paths{}, &stdout, &stderr); code != 0 || err != nil {
		t.Fatalf("help: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout.String(), "\n  create ") || !strings.Contains(stdout.String(), "--claim-credentials") {
		t.Fatalf("workspace usage does not advertise create and credential claiming: %q", stdout.String())
	}
}

func TestCredentialSessionRefreshWarnsAndContinuesWithOldDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zkad.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		var req request
		if decodeErr := json.NewDecoder(conn).Decode(&req); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		if req.Op != "credential_session_refresh" {
			serverErr <- errors.New("unexpected operation " + req.Op)
			return
		}
		serverErr <- json.NewEncoder(conn).Encode(response{
			Version: daemonProtocolVersion,
			OK:      false,
			Error:   `unknown operation "credential_session_refresh"`,
		})
	}()

	var stderr bytes.Buffer
	refreshCredentialSessionForCLI(NewAPI(Paths{Socket: path}), "work", "claim", "workspace", &stderr)
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "does not support graphical pinentry refresh") {
		t.Fatalf("warning = %q", stderr.String())
	}
}

func TestWorkspaceCreateSurfacesTemplateErrorsBeforeAnyRPC(t *testing.T) {
	// Paths{} has no reachable socket, so any error that mentions the
	// template proves parsing happened before an RPC was attempted.
	var stdout, stderr bytes.Buffer
	code, err := runWorkspace([]string{"create", "api", "--template", "/definitely/missing.session"}, Paths{}, &stdout, &stderr)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "read Kitty template") {
		t.Fatalf("missing template: code=%d err=%v", code, err)
	}
	unsafe := filepath.Join(t.TempDir(), "bad.session")
	if err := os.WriteFile(unsafe, []byte("detach_window\nlaunch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err = runWorkspace([]string{"create", "api", "--template", unsafe}, Paths{}, &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "not topology-safe") {
		t.Fatalf("unsafe template: code=%d err=%v", code, err)
	}
}

func TestCreationCredentialBundleDefaultsAndOptOut(t *testing.T) {
	var cfg Config
	cfg.Credentials.DefaultBundle = "work"
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{"work": {}, "personal": {}}
	for _, test := range []struct {
		name            string
		explicit        string
		noCredentials   bool
		compatibility   bool
		want            string
		wantErrContains string
	}{
		{name: "creator default", want: "work"},
		{name: "explicit override", explicit: "personal", want: "personal"},
		{name: "explicit opt out", noCredentials: true},
		{name: "conflicting opt out", explicit: "work", noCredentials: true, wantErrContains: "cannot be combined"},
		{name: "unknown override", explicit: "missing", wantErrContains: "not configured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := creationCredentialBundle(cfg, test.explicit, test.noCredentials, test.compatibility)
			if test.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("creationCredentialBundle() error = %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("creationCredentialBundle() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestWorkspaceCreateLocalCreatesDormantWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	var stdout, stderr bytes.Buffer
	code, err := runWorkspace([]string{"create", "api"}, d.paths, &stdout, &stderr)
	if code != 0 || err != nil {
		t.Fatalf("create: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 2 || fields[1] != "api" {
		t.Fatalf("create output = %q", stdout.String())
	}
	workspace, err := d.getWorkspace(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Attachments) != 0 || workspace.Topology.Generation != 1 {
		t.Fatalf("created workspace = attachments %d, generation %d", len(workspace.Attachments), workspace.Topology.Generation)
	}
	stdout.Reset()
	if code, err := runWorkspace([]string{"list"}, d.paths, &stdout, &stderr); code != 0 || err != nil {
		t.Fatalf("list: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout.String(), "dormant") {
		t.Fatalf("list does not mark the workspace dormant: %q", stdout.String())
	}
	stdout.Reset()
	if code, err := runWorkspace([]string{"inspect", fields[0]}, d.paths, &stdout, &stderr); code != 0 || err != nil {
		t.Fatalf("inspect: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout.String(), "dormant=true") {
		t.Fatalf("inspect does not report dormancy: %q", stdout.String())
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
	if strings.Contains(stdout.String(), "\t") ||
		!strings.Contains(stdout.String(), "\n  workspace   Manage workspace views") {
		t.Fatalf("top-level usage has inconsistent command indentation: %q", stdout.String())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), runner)
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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

func TestWorkspaceForgetCLIUsesOnlyTheLocalRemoteCache(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "stale", Panes: map[string]*Pane{},
		Attachments: map[string]*Attachment{"local": {
			ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: AttachmentDetached,
		}},
	}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)

	var stdout, stderr bytes.Buffer
	code, err := runWorkspaceForget([]string{"devbox.example:" + remote.ID[:8]}, d.paths, &stdout, &stderr)
	if err != nil || code != 0 || stdout.String() != remote.ID+"\tstale\tdevbox.example\n" {
		t.Fatalf("qualified forget: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	for _, call := range runner.Calls() {
		if call.Name == "ssh" {
			t.Fatalf("forget opened SSH: %#v", call)
		}
	}

	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code, err = runWorkspaceForget([]string{"stale"}, d.paths, &stdout, &stderr)
	if err != nil || code != 0 || stdout.String() != remote.ID+"\tstale\tdevbox.example\n" {
		t.Fatalf("unqualified forget: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}

	local := createTestWorkspace(t, d, 1)
	stdout.Reset()
	stderr.Reset()
	code, err = runWorkspaceForget([]string{local.ID}, d.paths, &stdout, &stderr)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "use workspace kill") {
		t.Fatalf("local forget: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
}

func TestWorkspaceForgetCLIValidatesCachedReference(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code, err := runWorkspaceForget(nil, Paths{}, &stdout, &stderr); code != 2 || err == nil {
		t.Fatalf("missing ref: code=%d err=%v", code, err)
	}
	if code, err := runWorkspaceForget([]string{"devbox.example:"}, Paths{}, &stdout, &stderr); code != 2 || err == nil {
		t.Fatalf("empty qualified ref: code=%d err=%v", code, err)
	}

	workspaces := []*Workspace{
		{ID: remoteWorkspaceIDForTest, Name: "duplicate", RemoteHost: "devbox.example"},
		{ID: secondRemoteWorkspaceIDForTest, Name: "duplicate", RemoteHost: "devbox.example"},
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "duplicate", RemoteHost: "other.example"},
	}
	if got, err := resolveCachedWorkspaceFromCopies(workspaces, "devbox.example", remoteWorkspaceIDForTest); err != nil || got.ID != remoteWorkspaceIDForTest {
		t.Fatalf("exact id resolution = %#v, %v", got, err)
	}
	if _, err := resolveCachedWorkspaceFromCopies(workspaces, "devbox.example", "duplicate"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous resolution error = %v", err)
	}
	if _, err := resolveCachedWorkspaceFromCopies(workspaces, "missing.example", "duplicate"); err == nil || !strings.Contains(err.Error(), "unknown cached workspace") {
		t.Fatalf("wrong host resolution error = %v", err)
	}
}

func TestWorkspaceReconcileHeadlessReconcilesBackends(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"headless":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	var stdout, stderr bytes.Buffer
	code, err := runWorkspace([]string{"reconcile", workspace.ID}, d.paths, &stdout, &stderr)
	if code != 0 || err != nil {
		t.Fatalf("headless reconcile: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout.String(), "backends reconciled (headless origin") {
		t.Fatalf("headless reconcile output = %q", stdout.String())
	}
	// An explicit attachment id keeps the normal path and its honest error.
	if code, err := runWorkspace([]string{"reconcile", workspace.ID, "--attachment", "missing"}, d.paths, &stdout, &stderr); code != 1 || err == nil {
		t.Fatalf("explicit attachment: code=%d err=%v", code, err)
	}
}
