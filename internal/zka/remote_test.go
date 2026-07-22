package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteControlHelloAndWorkspaceSnapshot(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	clientReader, serverInput := io.Pipe()
	serverOutput, clientWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- runRemoteControl(context.Background(), d.paths, clientReader, clientWriter) }()
	reader := bufio.NewReader(serverOutput)
	hello, err := readRemoteEnvelope(reader)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Type != "hello" || hello.Protocol != remoteProtocolName || hello.Version != protocolVersion {
		t.Fatalf("hello = %#v", hello)
	}
	payload, _ := json.Marshal(refRequest{Ref: workspace.ID})
	request := remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "request", ID: "7", Op: "get", Payload: payload}
	if err := json.NewEncoder(serverInput).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response remoteEnvelope
	for response.ID != "7" {
		response, err = readRemoteEnvelope(reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	var got Workspace
	if err := json.Unmarshal(response.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != workspace.ID {
		t.Fatalf("workspace = %#v", got)
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	_ = serverInput.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote control did not stop")
	}
}

func TestRemoteAgentForwardingDiagnosticOmitsSocketPath(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "agent.sock")
	listenTestAgent(t, socket, "agent")
	config := filepath.Join(root, "config.json")
	if err := os.WriteFile(config, []byte(`{"ssh":{"forward_agent":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", config)
	t.Setenv("SSH_AUTH_SOCK", socket)
	payload, err := dispatchRemoteControl(context.Background(), API{}, "agent_forwarding", nil)
	if err != nil {
		t.Fatal(err)
	}
	var status remoteAgentForwardingStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.ForwardedSocket || status.RelayVersion != agentRelayVersion {
		t.Fatalf("diagnostic = %#v", status)
	}
	if strings.Contains(string(payload), socket) {
		t.Fatal("diagnostic exposed forwarded socket path")
	}
}

func TestRemoteControlRejectsVersionMismatch(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	clientReader, serverInput := io.Pipe()
	serverOutput, clientWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- runRemoteControl(context.Background(), d.paths, clientReader, clientWriter) }()
	reader := bufio.NewReader(serverOutput)
	if _, err := readRemoteEnvelope(reader); err != nil {
		t.Fatal(err)
	}
	_ = json.NewEncoder(serverInput).Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: 1, Type: "request", ID: "bad", Op: "list"})
	response, err := readRemoteEnvelope(reader)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "bad" || !strings.Contains(response.Error, "incompatible") {
		t.Fatalf("response = %#v", response)
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	_ = serverInput.Close()
	<-done
}

func TestRemoteControlRenamesAndKillsAuthoritativeWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	api := NewAPI(d.paths)

	renamePayload, _ := json.Marshal(renameWorkspaceRequest{
		Workspace: workspace.ID, Name: "shell-work", ExpectedRevision: workspace.Revision,
	})
	raw, err := dispatchRemoteControl(context.Background(), api, "rename_workspace", renamePayload)
	if err != nil {
		t.Fatal(err)
	}
	var renamed Workspace
	if err := json.Unmarshal(raw, &renamed); err != nil || renamed.Name != "shell-work" {
		t.Fatalf("renamed workspace = %#v, %v", renamed, err)
	}

	killPayload, _ := json.Marshal(killWorkspaceRequest{WorkspaceID: workspace.ID})
	raw, err = dispatchRemoteControl(context.Background(), api, "kill_workspace", killPayload)
	if err != nil {
		t.Fatal(err)
	}
	var deleted workspaceDeletionResponse
	if err := json.Unmarshal(raw, &deleted); err != nil || deleted.DeletedWorkspaceID != workspace.ID || deleted.Name != "shell-work" {
		t.Fatalf("deletion response = %#v, %v", deleted, err)
	}
	// A lost response can be replayed by stable id on the same daemon.
	if _, err := dispatchRemoteControl(context.Background(), api, "kill_workspace", killPayload); err != nil {
		t.Fatalf("replayed kill: %v", err)
	}
}

func TestRemoteMessageLimit(t *testing.T) {
	oversized := strings.Repeat("x", remoteProtocolMax+1) + "\n"
	if _, err := readRemoteEnvelope(bufio.NewReader(strings.NewReader(oversized))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestSSHHostAliasIsSafeForKittyShellCommand(t *testing.T) {
	for _, good := range []string{"devbox.example", "user@devbox.example", "host-alias.example"} {
		if err := validateSSHHost(good); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"", "-option", "devbox.example;touch", "devbox.example name", "devbox.example'"} {
		if err := validateSSHHost(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestRemoteCachePreservesLocalRuntimeMapping(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: "remote", Name: "example-project", Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 4,
		Panes:       map[string]*Pane{"pane": {ID: "pane", Visible: true}},
		Attachments: map[string]*Attachment{},
	}
	d.cacheRemoteWorkspace("devbox.example", remote)
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["local"] = &Attachment{ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Views: readyView("pane", 9), Status: AttachmentReady}
	d.mu.Unlock()
	remote.Revision = 5
	remote.Attachments["local"] = &Attachment{ID: "local", Endpoint: "ssh:laptop.example", Role: AttachmentPrimary, AppliedRevision: 5}
	cached := d.cacheRemoteWorkspace("devbox.example", remote)
	local := cached.Attachments["local"]
	if local.Endpoint != "unix:/kitty" || local.Views["pane"].WindowID != 9 || local.Role != AttachmentPrimary || local.AppliedRevision != 5 {
		t.Fatalf("local attachment = %#v", local)
	}
}

func TestRemoteSnapshotEvictsMissingWorkspaceAndClosesLocalView(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: "remote", Name: "example-project", Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 4,
		Panes: map[string]*Pane{"pane": {ID: "pane", Visible: true}}, Attachments: map[string]*Attachment{},
	}
	d.cacheRemoteWorkspace("devbox.example", remote)
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["local"] = &Attachment{
		ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: AttachmentReady,
	}
	d.mu.Unlock()
	d.cacheRemoteSnapshot("devbox.example", nil)
	if _, err := d.getWorkspace(remote.ID); err == nil {
		t.Fatal("workspace missing from a full snapshot remained cached")
	}
	d.mu.Lock()
	_, cached := d.state.Remotes["devbox.example"].Workspaces[remote.ID]
	d.mu.Unlock()
	if cached {
		t.Fatal("remote cache retained an absent workspace")
	}
	waitFor(t, func() bool {
		for _, call := range runner.Calls() {
			if call.Name == "kitten" && strings.Contains(strings.Join(call.Args, " "), "close-window") {
				return true
			}
		}
		return false
	})
}

func TestUnreachableSSHControlReturnsWithoutMutatingWorkspace(t *testing.T) {
	t.Setenv("ZKA_SSH_COMMAND", "/definitely/missing/zka-ssh")
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = d.remotes.Call(ctx, "devbox.example", "list", nil)
	if err == nil || !strings.Contains(err.Error(), "start SSH") {
		t.Fatalf("error = %v", err)
	}
	if len(d.state.Workspaces) != 0 {
		t.Fatalf("unexpected state mutation: %#v", d.state.Workspaces)
	}
}

func TestInitialSSHExit255ReturnsDiagnostic(t *testing.T) {
	t.Setenv("GO_WANT_ZKA_SSH_HELPER", "exit255")
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1234/agent-a.socket")
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1234/agent-b.socket")
	d.config.SSH.Command = os.Args[0]
	d.config.SSH.Options = []string{"-test.run=TestZKASSHHelperProcess", "--"}
	var logs boundedTailBuffer
	d.logger.SetOutput(&logs)
	serveTestDaemon(t, d)
	api := NewAPI(d.paths)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err = api.RemoteCall(ctx, "devbox.example", "list", nil, new([]*Workspace))
	if err == nil || !strings.Contains(err.Error(), "status 255") || !strings.Contains(err.Error(), "Permission denied (publickey)") || !strings.Contains(err.Error(), "SSH agent mismatch") {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatalf("initial authentication failure was retried for %s", time.Since(started))
	}
	if !strings.Contains(logs.String(), "Permission denied (publickey)") {
		t.Fatalf("daemon log = %q", logs.String())
	}
	if retryErr := api.RemoteCall(ctx, "devbox.example", "list", nil, new([]*Workspace)); retryErr == nil {
		t.Fatal("retry unexpectedly succeeded")
	}
	if strings.Count(logs.String(), "Permission denied (publickey)") != 2 {
		t.Fatalf("a corrected agent could not start a fresh SSH attempt; daemon log = %q", logs.String())
	}
}

func TestRemoteCallDeadlineReturnsDaemonErrorInsteadOfSocketTimeout(t *testing.T) {
	t.Setenv("GO_WANT_ZKA_SSH_HELPER", "hang")
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = os.Args[0]
	d.config.SSH.Options = []string{"-test.run=TestZKASSHHelperProcess", "--"}
	serveTestDaemon(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = NewAPI(d.paths).RemoteCall(ctx, "devbox.example", "list", nil, new([]*Workspace))
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "read response") || strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("remote failure was obscured by local socket error: %v", err)
	}
	d.remotes.mu.Lock()
	_, retained := d.remotes.clients["devbox.example"]
	d.remotes.mu.Unlock()
	if retained {
		t.Fatal("timed-out initial SSH process remained cached and blocked a fresh attempt")
	}
}

func TestEstablishedSSHExit255RetriesAndPreservesLastFailure(t *testing.T) {
	t.Setenv("GO_WANT_ZKA_SSH_HELPER", "hello-exit255")
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = os.Args[0]
	d.config.SSH.Options = []string{"-test.run=TestZKASSHHelperProcess", "--"}
	var logs boundedTailBuffer
	d.logger.SetOutput(&logs)

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	_, err = d.remotes.Call(ctx, "devbox.example", "list", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "status 255") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error = %v", err)
	}
	if strings.Count(logs.String(), "connection reset") < 2 {
		t.Fatalf("established connection was not retried; daemon log = %q", logs.String())
	}
}

func TestZKASSHHelperProcess(t *testing.T) {
	switch os.Getenv("GO_WANT_ZKA_SSH_HELPER") {
	case "exit255":
		_, _ = io.WriteString(os.Stderr, "sign_and_send_pubkey: signing failed: agent refused operation\nPermission denied (publickey).\n")
		os.Exit(255)
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	case "hello-exit255":
		_ = json.NewEncoder(os.Stdout).Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "hello"})
		_, _ = io.WriteString(os.Stderr, "client_loop: send disconnect: connection reset\n")
		time.Sleep(25 * time.Millisecond)
		os.Exit(255)
	}
}

func TestSSHStderrCaptureIsBoundedAndKeepsTail(t *testing.T) {
	var stderr boundedTailBuffer
	prefix := strings.Repeat("x", maxSSHStderr+100)
	_, _ = stderr.Write([]byte(prefix))
	_, _ = stderr.Write([]byte("Permission denied (publickey)."))
	detail := stderr.String()
	if !strings.Contains(detail, "stderr truncated") || !strings.HasSuffix(detail, "Permission denied (publickey).") {
		t.Fatalf("captured stderr = %q", detail)
	}
	if len(detail) > maxSSHStderr+100 {
		t.Fatalf("captured %d bytes, want bounded near %d", len(detail), maxSSHStderr)
	}
}

func TestClientHeartbeatFreshness(t *testing.T) {
	now := time.Now().UTC()
	if !clientHeartbeatFresh(now.Add(-5*time.Second), now) {
		t.Fatal("fresh heartbeat rejected")
	}
	if clientHeartbeatFresh(now.Add(-7*time.Second), now) || clientHeartbeatFresh(time.Time{}, now) {
		t.Fatal("stale heartbeat accepted")
	}
}

func TestRemotePaneReadinessComesFromOriginClientHeartbeat(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "remote-view", Node: Host{ID: "laptop.example", Name: "laptop.example"},
		Transport: Transport{Kind: "ssh", Host: "devbox.example"}, Endpoint: "ssh:laptop.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "test", PID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.setAttachmentPaneReady(attachmentPaneReadyRequest{Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID, Ready: true}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(paneReadinessRequest{Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID})
	raw, err := dispatchRemoteControl(context.Background(), NewAPI(d.paths), "pane_readiness", payload)
	if err != nil {
		t.Fatal(err)
	}
	var ready paneReadinessResponse
	if err := json.Unmarshal(raw, &ready); err != nil {
		t.Fatal(err)
	}
	if !ready.BackendReady || !ready.ClientReady {
		t.Fatalf("readiness = %#v", ready)
	}
}

func TestRemoteDeadPaneIsReadyWhilePlaceholderClientIsAlive(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	attachment, err := d.registerAttachment(workspace.ID, Attachment{
		ID: "remote-dead-view", Node: Host{ID: "laptop.example", Name: "laptop.example"},
		Transport: Transport{Kind: "ssh", Host: "devbox.example"}, Endpoint: "ssh:laptop.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.applyEvent(context.Background(), Event{
		WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "backend_error", Source: "zmx", Detail: "session missing",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.setAttachmentPaneReady(attachmentPaneReadyRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID, Ready: true,
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(paneReadinessRequest{Workspace: workspace.ID, Attachment: attachment.ID, Pane: pane.ID})
	raw, err := dispatchRemoteControl(context.Background(), NewAPI(d.paths), "pane_readiness", payload)
	if err != nil {
		t.Fatal(err)
	}
	var ready paneReadinessResponse
	if err := json.Unmarshal(raw, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.BackendReady || !ready.BackendDead || !ready.ClientReady {
		t.Fatalf("dead pane readiness = %#v", ready)
	}
}
