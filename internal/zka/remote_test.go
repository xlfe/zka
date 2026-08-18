package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	remoteWorkspaceIDForTest       = "0123456789abcdef0123456789abcdef"
	secondRemoteWorkspaceIDForTest = "fedcba9876543210fedcba9876543210"
)

func TestRemoteControlHelloAndWorkspaceSnapshot(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	if hello.Type != "hello" || hello.Protocol != remoteProtocolName || hello.Version != remoteProtocolVersion {
		t.Fatalf("hello = %#v", hello)
	}
	var serverHello remoteServerHello
	if err := json.Unmarshal(hello.Payload, &serverHello); err != nil || serverHello.Node.ID != d.state.Node.ID {
		t.Fatalf("server identity = %#v, %v", serverHello, err)
	}
	payload, _ := json.Marshal(refRequest{Ref: workspace.ID})
	request := remoteEnvelope{Protocol: remoteProtocolName, Version: remoteProtocolVersion, Type: "request", ID: "7", Op: "get", Payload: payload}
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

func TestRemoteControlRejectsVersionMismatch(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	var response remoteEnvelope
	for response.ID != "bad" || response.Type != "response" {
		response, err = readRemoteEnvelope(reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	if response.ID != "bad" || !strings.Contains(response.Error, "incompatible") {
		t.Fatalf("response = %#v", response)
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	_ = serverInput.Close()
	<-done
}

func TestRemoteControlRenamesAndKillsAuthoritativeWorkspace(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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

func TestRemoteControlCreatesWorkspaceIdempotently(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	api := NewAPI(d.paths)
	key := "00112233445566778899aabbccddeeff"
	payload, _ := json.Marshal(createWorkspaceRequest{Name: "  shell-work  ", CreationKey: key})
	raw, err := dispatchRemoteControl(context.Background(), api, "create_workspace", payload)
	if err != nil {
		t.Fatal(err)
	}
	var created Workspace
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "shell-work" || created.RemoteHost != "" || created.Origin.ID != d.state.Node.ID {
		t.Fatalf("created = %#v", created)
	}
	if len(created.Shell) == 0 || created.Shell[0] != "fish" {
		t.Fatalf("origin shell default was not applied: %#v", created.Shell)
	}
	// A replay of the identical payload — what the remote-call retry loop
	// sends after a dropped response — returns the same workspace.
	raw, err = dispatchRemoteControl(context.Background(), api, "create_workspace", payload)
	if err != nil {
		t.Fatal(err)
	}
	var replayed Workspace
	if err := json.Unmarshal(raw, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID {
		t.Fatalf("replay created a second workspace: %s != %s", replayed.ID, created.ID)
	}
	d.mu.Lock()
	count := len(d.state.Workspaces)
	d.mu.Unlock()
	if count != 1 {
		t.Fatalf("workspace count after replay = %d", count)
	}
	// A different invocation reusing the name is a genuine collision.
	payload, _ = json.Marshal(createWorkspaceRequest{Name: "shell-work", CreationKey: "ffeeddccbbaa99887766554433221100"})
	if _, err := dispatchRemoteControl(context.Background(), api, "create_workspace", payload); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v", err)
	}
	// A genesis topology travels the wire and is attachable on arrival.
	plan, err := TemplateGenesis(mustParseTemplate(t, "new_tab work\nlayout splits\nlaunch\nlaunch\n"), "/work")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(createWorkspaceRequest{
		Name: "genesis", Panes: plan.Panes, Topology: plan.Topology, CreationKey: "aabbccddeeff00112233445566778899",
	})
	raw, err = dispatchRemoteControl(context.Background(), api, "create_workspace", payload)
	if err != nil {
		t.Fatal(err)
	}
	var genesis Workspace
	if err := json.Unmarshal(raw, &genesis); err != nil {
		t.Fatal(err)
	}
	if genesis.Topology.Generation != 1 || strings.TrimSpace(genesis.Manifest.Session) == "" {
		t.Fatalf("genesis over the wire = generation %d, session %q", genesis.Topology.Generation, genesis.Manifest.Session)
	}
}

func TestRemoteCreateResponseIsCachedOnDestination(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "born-remote",
		Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 1,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
	}
	raw, _ := json.Marshal(remote)
	// Without this cacheResult case the follow-up attach cannot resolve the
	// workspace it just created.
	if err := d.remotes.cacheResult("devbox.example", "create_workspace", raw); err != nil {
		t.Fatal(err)
	}
	got, err := d.getWorkspace(remoteWorkspaceIDForTest)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteHost != "devbox.example" {
		t.Fatalf("cached workspace = %#v", got)
	}
	d.mu.Lock()
	_, cached := d.state.Remotes["devbox.example"].Workspaces[remoteWorkspaceIDForTest]
	d.mu.Unlock()
	if !cached {
		t.Fatal("create response missing from the remote cache")
	}
}

func TestRemoteCachePreservesLocalRuntimeMapping(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "example-project", Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 4,
		Panes:       map[string]*Pane{"pane": {ID: "pane", Phase: PaneAdmitted}},
		Attachments: map[string]*Attachment{},
	}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["local"] = &Attachment{ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Views: readyView("pane", 9), Status: AttachmentReady}
	d.mu.Unlock()
	remote.Revision = 5
	remote.Attachments["local"] = &Attachment{ID: "local", Endpoint: "ssh:laptop.example", Role: AttachmentPrimary, AppliedRevision: 5}
	cached, err := d.cacheRemoteWorkspace("devbox.example", remote)
	if err != nil {
		t.Fatal(err)
	}
	local := cached.Attachments["local"]
	if local.Endpoint != "unix:/kitty" || local.Views["pane"].WindowID != 9 || local.Role != AttachmentPrimary ||
		local.AppliedRevision != 0 || local.AppliedTopologyGeneration != 0 {
		t.Fatalf("local attachment = %#v", local)
	}
}

func TestRemoteCacheKeepsDetachedAttachmentDetached(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "example-project", Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 4,
		Topology:    DesiredTopology{Generation: 1, Digest: "aaaa"},
		Panes:       map[string]*Pane{"pane": {ID: "pane", Phase: PaneAdmitted}},
		Attachments: map[string]*Attachment{},
	}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	// The local view is gone and its detach RPC never reached the origin, so
	// the authoritative copy still shows the attachment as ready.
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["local"] = &Attachment{
		ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: AttachmentDetached,
	}
	d.mu.Unlock()
	remote.Revision = 5
	remote.Topology = DesiredTopology{Generation: 2, Digest: "bbbb"}
	remote.Attachments["local"] = &Attachment{ID: "local", Endpoint: "ssh:laptop.example", Status: AttachmentReady, AppliedRevision: 5}
	cached, err := d.cacheRemoteWorkspace("devbox.example", remote)
	if err != nil {
		t.Fatal(err)
	}
	// A topology bump must not resurrect the detached attachment to Preparing:
	// there is no Kitty behind it, and pendingDetaches only re-sends the origin
	// detach while the local status stays Detached.
	if got := cached.Attachments["local"].Status; got != AttachmentDetached {
		t.Fatalf("detached attachment resurrected to %q by a topology bump", got)
	}
}

func TestRemoteSnapshotEvictsMissingWorkspaceAndClosesLocalView(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "example-project", Origin: Host{ID: "devbox.example", Name: "devbox.example"}, Revision: 4,
		Panes: map[string]*Pane{"pane": {ID: "pane", Phase: PaneAdmitted}}, Attachments: map[string]*Attachment{},
	}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Workspaces[remote.ID].Attachments["local"] = &Attachment{
		ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: AttachmentReady,
	}
	d.mu.Unlock()
	if err := d.cacheRemoteSnapshot("devbox.example", nil); err != nil {
		t.Fatal(err)
	}
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

func TestForgetDetachedRemoteWorkspaceCleansOnlyLocalState(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	host := "devbox.example"
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "stale", Origin: Host{ID: "origin", Name: host}, Revision: 4,
		Panes: map[string]*Pane{"pane": {ID: "pane", Phase: PaneAdmitted}},
		Attachments: map[string]*Attachment{"local": {
			ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: AttachmentDetached,
		}},
	}
	sibling := &Workspace{
		ID: secondRemoteWorkspaceIDForTest, Name: "keep", Origin: Host{ID: "origin", Name: host}, Revision: 2,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
	}
	if _, err := d.cacheRemoteWorkspace(host, remote); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cacheRemoteWorkspace(host, sibling); err != nil {
		t.Fatal(err)
	}
	sessionPath, err := d.store.WriteSession(remote.ID, "local", "launch\n")
	if err != nil {
		t.Fatal(err)
	}

	if err := d.deleteWorkspace(remote.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.getWorkspace(remote.ID); err == nil {
		t.Fatal("forgotten remote workspace remained in the local workspace list")
	}
	if _, err := d.getWorkspace(sibling.ID); err != nil {
		t.Fatalf("sibling workspace was removed: %v", err)
	}
	d.mu.Lock()
	cache := d.state.Remotes[host]
	_, forgottenCached := cache.Workspaces[remote.ID]
	_, siblingCached := cache.Workspaces[sibling.ID]
	d.mu.Unlock()
	if forgottenCached || !siblingCached {
		t.Fatalf("remote cache after forget = %#v", cache.Workspaces)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated session still exists: %v", err)
	}
	if withdrawn := fakeDesktop(t, d).Withdrawn(); len(withdrawn) != 1 || withdrawn[0].Workspace != remote.ID || withdrawn[0].Pane != "pane" {
		t.Fatalf("withdrawn notifications = %#v", withdrawn)
	}

	if err := d.deleteWorkspace(sibling.ID); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, hostCached := d.state.Remotes[host]
	d.mu.Unlock()
	if hostCached {
		t.Fatal("empty remote host cache remained after forgetting its final workspace")
	}
	if _, err := d.cacheRemoteWorkspace(host, remote); err != nil {
		t.Fatalf("forgotten workspace could not be rediscovered: %v", err)
	}
	if got, err := d.getWorkspace(remote.ID); err != nil || got.RemoteHost != host {
		t.Fatalf("rediscovered workspace = %#v, %v", got, err)
	}
}

func TestForgetRemoteWorkspaceRequiresEveryLocalAttachmentDetached(t *testing.T) {
	for _, status := range []AttachmentStatus{AttachmentPreparing, AttachmentReady, AttachmentLayoutStalled, AttachmentUnhealthy} {
		t.Run(string(status), func(t *testing.T) {
			d, err := newTestDaemon(t, testRoot(t), quietRunner())
			if err != nil {
				t.Fatal(err)
			}
			remote := &Workspace{
				ID: remoteWorkspaceIDForTest, Name: "active", Panes: map[string]*Pane{},
				Attachments: map[string]*Attachment{"local": {
					ID: "local", Endpoint: "unix:/kitty", Node: d.state.Node, Status: status,
				}},
			}
			if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err != nil {
				t.Fatal(err)
			}
			if err := d.deleteWorkspace(remote.ID); err == nil || !strings.Contains(err.Error(), "detach it before forgetting") {
				t.Fatalf("forget error = %v", err)
			}
			if _, err := d.getWorkspace(remote.ID); err != nil {
				t.Fatalf("rejected workspace was removed: %v", err)
			}
			d.mu.Lock()
			_, cached := d.state.Remotes["devbox.example"].Workspaces[remote.ID]
			d.mu.Unlock()
			if !cached {
				t.Fatal("rejected workspace was removed from the remote cache")
			}
		})
	}
}

func TestForgetRemoteWorkspaceRestoresStateWhenPersistenceFails(t *testing.T) {
	root := testRoot(t)
	d, err := newTestDaemon(t, root, quietRunner())
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
	d.store.paths.StateFile = d.paths.StateDir
	if err := d.deleteWorkspace(remote.ID); err == nil {
		t.Fatal("forget succeeded despite an unwritable state target")
	}
	if _, err := d.getWorkspace(remote.ID); err != nil {
		t.Fatalf("workspace was not restored after save failure: %v", err)
	}
	d.mu.Lock()
	_, cached := d.state.Remotes["devbox.example"].Workspaces[remote.ID]
	d.mu.Unlock()
	if !cached {
		t.Fatal("remote cache was not restored after save failure")
	}
}

func TestRemoteCacheRejectsAuthoritativeLocalWorkspaceCollision(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	local := createTestWorkspace(t, d, 1)
	spoof := local.Clone()
	spoof.Name = "spoofed-remote-name"
	if _, err := d.cacheRemoteWorkspace("devbox.example", spoof); err == nil || !strings.Contains(err.Error(), "authoritative local workspace") {
		t.Fatalf("collision error = %v", err)
	}
	got, err := d.getWorkspace(local.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteHost != "" || got.Name != local.Name {
		t.Fatalf("local workspace was mutated: %#v", got)
	}
}

func TestRemoteSnapshotRejectsCollisionBeforeCachingAnyEntry(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	local := createTestWorkspace(t, d, 1)
	safeRemote := &Workspace{
		ID: secondRemoteWorkspaceIDForTest, Name: "safe-remote",
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
	}
	spoof := local.Clone()
	if err := d.cacheRemoteSnapshot("devbox.example", []*Workspace{safeRemote, spoof}); err == nil || !strings.Contains(err.Error(), "authoritative local workspace") {
		t.Fatalf("snapshot collision error = %v", err)
	}
	if _, err := d.getWorkspace(safeRemote.ID); err == nil {
		t.Fatal("snapshot cached an earlier entry before rejecting a collision")
	}
	got, err := d.getWorkspace(local.ID)
	if err != nil || got.RemoteHost != "" {
		t.Fatalf("local workspace after rejected snapshot = %#v, %v", got, err)
	}
}

func TestRemoteDeletionCannotRemoveAuthoritativeLocalSessions(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	local := createTestWorkspace(t, d, 1)
	sessionPath, err := d.store.WriteSession(local.ID, "local", "launch\n")
	if err != nil {
		t.Fatal(err)
	}
	d.evictRemoteWorkspace("devbox.example", local.ID)
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("remote deletion removed an authoritative local session: %v", err)
	}
	got, err := d.getWorkspace(local.ID)
	if err != nil || got.RemoteHost != "" {
		t.Fatalf("local workspace after remote deletion = %#v, %v", got, err)
	}
}

func TestRemoteCacheRejectsCrossHostWorkspaceCollision(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{
		ID: remoteWorkspaceIDForTest, Name: "example-project", Revision: 1,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
	}
	if _, err := d.cacheRemoteWorkspace("first.example", remote); err != nil {
		t.Fatal(err)
	}
	spoof := remote.Clone()
	spoof.Name = "second-host-name"
	if _, err := d.cacheRemoteWorkspace("second.example", spoof); err == nil || !strings.Contains(err.Error(), "cached from first.example") {
		t.Fatalf("collision error = %v", err)
	}
	got, err := d.getWorkspace(remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteHost != "first.example" || got.Name != remote.Name {
		t.Fatalf("first remote workspace was mutated: %#v", got)
	}
}

func TestRemoteCacheRejectsMalformedWorkspaceID(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	remote := &Workspace{ID: "../../local-*", Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{}}
	if _, err := d.cacheRemoteWorkspace("devbox.example", remote); err == nil || !strings.Contains(err.Error(), "lowercase hexadecimal") {
		t.Fatalf("validation error = %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.state.Workspaces) != 0 || len(d.state.Remotes) != 0 {
		t.Fatalf("malformed remote workspace mutated state: %#v", d.state)
	}
}

func TestUnreachableSSHControlReturnsWithoutMutatingWorkspace(t *testing.T) {
	t.Setenv("ZKA_SSH_COMMAND", "/definitely/missing/zka-ssh")
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	if status := d.remotes.credentialTransportStatusForHost("devbox.example"); status.State != "terminal" || !strings.Contains(status.LastError, "Permission denied") {
		t.Fatalf("terminal transport status = %#v", status)
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = os.Args[0]
	d.config.SSH.Options = []string{"-test.run=TestZKASSHHelperProcess", "--"}
	var logs boundedTailBuffer
	d.logger.SetOutput(&logs)

	// Leave enough room for two jittered reconnects even under a loaded Nix
	// builder; the production backoff intentionally grows to 30 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = d.remotes.Call(ctx, "devbox.example", "list", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "status 255") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("error = %v", err)
	}
	if strings.Count(logs.String(), "connection reset") < 2 {
		t.Fatalf("established connection was not retried; daemon log = %q", logs.String())
	}
}

func TestRemoteSSHTerminalClassifiesAuthenticationAndHostTrustFailures(t *testing.T) {
	for _, detail := range []string{
		"Permission denied (publickey).",
		"Host key verification failed.",
		"WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!",
		"no matching host key type found",
	} {
		if !remoteSSHTerminal(errors.New("ssh failed"), detail) {
			t.Fatalf("terminal SSH failure was classified as retryable: %q", detail)
		}
	}
	if remoteSSHTerminal(errors.New("ssh failed"), "Connection reset by peer") {
		t.Fatal("transient connection reset was classified as terminal")
	}
}

func TestCredentialProviderIdentityBindsNodeToSSHSource(t *testing.T) {
	const nodeID = "fedcba9876543210fedcba9876543210"
	cfg := defaultConfig()
	cfg.Credentials.Providers["laptop"] = CredentialProviderConfig{
		NodeID: nodeID, SSHSourceAddresses: []string{"192.0.2.10", "2001:db8::/64"},
	}
	for _, connection := range []string{
		"192.0.2.10 42000 192.0.2.20 22",
		"2001:db8::42 42000 2001:db8::99 22",
	} {
		if err := authenticateCredentialProvider(cfg, Host{ID: nodeID}, connection); err != nil {
			t.Fatalf("connection %q: %v", connection, err)
		}
	}
	for _, test := range []struct {
		node, connection string
	}{
		{"0123456789abcdef0123456789abcdef", "192.0.2.10 42000 192.0.2.20 22"},
		{nodeID, "198.51.100.10 42000 192.0.2.20 22"},
		{nodeID, ""},
	} {
		if err := authenticateCredentialProvider(cfg, Host{ID: test.node}, test.connection); err == nil {
			t.Fatalf("unauthenticated provider accepted: %#v", test)
		}
	}
	cfg.Credentials.Providers["laptop"] = CredentialProviderConfig{NodeID: nodeID}
	if err := authenticateCredentialProvider(cfg, Host{ID: nodeID}, ""); err != nil {
		t.Fatalf("roaming provider with no address restriction: %v", err)
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
		session, err := yamux.Server(newStdioConn(os.Stdin, os.Stdout), remoteYamuxConfig())
		if err != nil {
			os.Exit(2)
		}
		control, err := session.AcceptStream()
		if err != nil {
			os.Exit(2)
		}
		payload, _ := json.Marshal(remoteServerHello{Node: Host{ID: "0123456789abcdef0123456789abcdef"}})
		_ = json.NewEncoder(control).Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: remoteProtocolVersion, Type: "hello", Payload: payload})
		// Do not exit until the real client has accepted the hello and written its
		// response; otherwise a loaded builder can turn this established-session
		// retry test into an initial-handshake race.
		_, _ = readRemoteEnvelope(bufio.NewReader(control))
		_, _ = io.WriteString(os.Stderr, "client_loop: send disconnect: connection reset\n")
		time.Sleep(100 * time.Millisecond)
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
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
