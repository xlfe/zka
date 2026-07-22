package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func setPaneForNotification(t *testing.T, d *Daemon, workspace *Workspace, state AgentState, turn string) (*Workspace, *Pane) {
	return setPaneForNotificationWithDetail(t, d, workspace, state, turn, "")
}

func setPaneForNotificationWithDetail(t *testing.T, d *Daemon, workspace *Workspace, state AgentState, turn, detail string) (*Workspace, *Pane) {
	t.Helper()
	d.mu.Lock()
	actual := d.state.Workspaces[workspace.ID]
	var pane *Pane
	for _, candidate := range actual.Panes {
		pane = candidate
		break
	}
	pane.State = state
	pane.LastTurnID = turn
	pane.Evidence = Evidence{Source: "test", Event: "transition", Detail: detail, Timestamp: time.Now().UTC()}
	actual.RecomputeAttention()
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	copy := actual.Clone()
	d.mu.Unlock()
	return copy, copy.Panes[pane.ID]
}

func TestNtfyEvidenceIsOptIn(t *testing.T) {
	const rawEvidence = "approve production deploy with token secret-value"
	for _, test := range []struct {
		name            string
		includeEvidence bool
	}{
		{name: "redacted by default", includeEvidence: false},
		{name: "included when configured", includeEvidence: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := quietRunner()
			d, err := newTestDaemon(t, t.TempDir(), runner)
			if err != nil {
				t.Fatal(err)
			}
			d.config.Notifications.NtfyIncludeEvidence = test.includeEvidence
			workspace := createTestWorkspace(t, d, 1)
			workspace, pane := setPaneForNotificationWithDetail(t, d, workspace, StateError, "turn-evidence", rawEvidence)
			d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
			call := firstCommand(runner.Calls(), "ntfy-send")
			body := call.Args[len(call.Args)-1]
			if strings.Contains(body, rawEvidence) != test.includeEvidence {
				t.Fatalf("ntfy body evidence mismatch: %q", body)
			}
			if !test.includeEvidence && !strings.Contains(body, "State: error") {
				t.Fatalf("redacted ntfy body lacks safe state summary: %q", body)
			}
		})
	}
}

func TestImportantDetachedStatesUseNtfy(t *testing.T) {
	for _, test := range []struct {
		state         AgentState
		priority, tag string
	}{
		{StateDone, "-p 3", "white_check_mark"},
		{StateBlocked, "-p 5", "warning"},
		{StateError, "-p 5", "rotating_light"},
	} {
		runner := quietRunner()
		d, err := newTestDaemon(t, t.TempDir(), runner)
		if err != nil {
			t.Fatal(err)
		}
		workspace := createTestWorkspace(t, d, 1)
		workspace, pane := setPaneForNotification(t, d, workspace, test.state, "turn-"+string(test.state))
		d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
		call := firstCommand(runner.Calls(), "ntfy-send")
		joined := strings.Join(call.Args, " ")
		if !strings.Contains(joined, test.priority) || !strings.Contains(joined, test.tag) {
			t.Fatalf("%s ntfy args = %#v", test.state, call.Args)
		}
	}
}

func TestNotificationDedupeIsPerPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	key := "ntfy:error:event"
	if !d.reserveNotification(workspace.ID, pane.ID, key, "ntfy") {
		t.Fatal("first reservation failed")
	}
	if d.reserveNotification(workspace.ID, pane.ID, key, "ntfy") {
		t.Fatal("duplicate reservation succeeded")
	}
}

func TestAttentionPauseDefersCurrentNotificationsUntilResume(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	if _, err := d.setAttentionPaused(true); err != nil {
		t.Fatal(err)
	}
	workspace, pane := setPaneForNotification(t, d, workspace, StateBlocked, "turn-paused")
	d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
	if hasCommand(runner.Calls(), "ntfy-send") {
		t.Fatal("paused attention delivered ntfy")
	}
	if _, err := d.setAttentionPaused(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hasCommand(runner.Calls(), "ntfy-send") })
	if _, err := d.setAttentionPaused(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	count := 0
	for _, call := range runner.Calls() {
		if call.Name == "ntfy-send" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("resume deliveries = %d, want 1", count)
	}
}

func TestAttentionResolvedWhilePausedDoesNotNotifyOnResume(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	if _, err := d.setAttentionPaused(true); err != nil {
		t.Fatal(err)
	}
	workspace, pane = setPaneForNotification(t, d, workspace, StateBlocked, "turn-resolved")
	d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
	workspace, pane = setPaneForNotification(t, d, workspace, StateWorking, "turn-resolved")
	d.afterTransition(context.Background(), StateBlocked, workspace, pane.ID)
	if _, err := d.setAttentionPaused(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if hasCommand(runner.Calls(), "ntfy-send") {
		t.Fatal("resolved attention item notified on resume")
	}
}

func TestNotificationChannelsCanBeDisabledIndependently(t *testing.T) {
	t.Run("ntfy", func(t *testing.T) {
		runner := quietRunner()
		d, err := newTestDaemon(t, t.TempDir(), runner)
		if err != nil {
			t.Fatal(err)
		}
		d.config.Notifications.NtfyEnabled = false
		workspace := createTestWorkspace(t, d, 1)
		workspace, pane := setPaneForNotification(t, d, workspace, StateError, "turn-disabled")
		d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
		if hasCommand(runner.Calls(), "ntfy-send") {
			t.Fatal("disabled ntfy channel was invoked")
		}
	})
	t.Run("desktop", func(t *testing.T) {
		runner := quietRunner()
		d, err := newTestDaemon(t, t.TempDir(), runner)
		if err != nil {
			t.Fatal(err)
		}
		d.config.Notifications.DesktopEnabled = false
		d.config.Notifications.NtfyEnabled = false
		workspace := createTestWorkspace(t, d, 1)
		pane := firstPane(workspace)
		d.mu.Lock()
		actual := d.state.Workspaces[workspace.ID]
		actual.Attachments["local"] = &Attachment{
			ID: "local", Endpoint: "unix:/kitty", Status: AttachmentReady,
			Views: readyView(pane.ID, 7),
		}
		if err := d.store.Save(d.state); err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
		workspace = actual.Clone()
		d.mu.Unlock()
		workspace, pane = setPaneForNotification(t, d, workspace, StateBlocked, "turn-desktop-disabled")
		d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
		for _, call := range runner.Calls() {
			if call.Name == "kitten" && strings.Contains(strings.Join(call.Args, " "), " notify ") {
				t.Fatalf("disabled desktop channel invoked Kitty notify: %#v", call.Args)
			}
		}
	})
}

func TestStatesExcludedFromAttentionDoNotNotify(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Attention.States = []AgentState{StateBlocked, StateError}
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := setPaneForNotification(t, d, workspace, StateDone, "turn-finished")
	d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
	if hasCommand(runner.Calls(), "ntfy-send") {
		t.Fatal("excluded done state delivered a notification")
	}
}

func TestRemoteMirrorUsesKittyNotificationButNeverDuplicatesNtfy(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	actual := d.state.Workspaces[workspace.ID]
	actual.RemoteHost = "devbox.example"
	actual.Panes[pane.ID].State = StateBlocked
	actual.Panes[pane.ID].LastTurnID = "remote-turn"
	actual.Attachments["local"] = &Attachment{
		ID: "local", Endpoint: "unix:/kitty", Status: AttachmentReady,
		Views: readyView(pane.ID, 7),
	}
	actual.Attachments["local"].Views[pane.ID] = RuntimeView{PaneID: pane.ID, WindowID: 7, Ready: true, Focused: false}
	copy := actual.Clone()
	d.mu.Unlock()
	d.afterRemoteTransition(context.Background(), copy, pane.ID)
	waitFor(t, func() bool {
		for _, call := range runner.Calls() {
			if call.Name == "kitten" && strings.Contains(strings.Join(call.Args, " "), " notify ") {
				return true
			}
		}
		return false
	})
	if hasCommand(runner.Calls(), "ntfy-send") {
		t.Fatal("remote mirror duplicated origin ntfy notification")
	}
}

func TestNtfyFailureIsRetriedAndRecorded(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
			return "[]", "", nil
		}
		if name == "ntfy-send" {
			return "", "", errors.New("token secret is unreadable")
		}
		return "", "", nil
	}}
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := setPaneForNotification(t, d, workspace, StateError, "turn-error")
	d.afterTransition(context.Background(), StateWorking, workspace, pane.ID)
	got, _ := d.getWorkspace(workspace.ID)
	records := got.Panes[pane.ID].Notifications
	found := false
	for _, record := range records {
		found = found || (record.Channel == "ntfy" && strings.Contains(record.LastError, "token secret"))
	}
	if !found {
		t.Fatalf("records = %#v", records)
	}
	count := 0
	for _, call := range runner.Calls() {
		if call.Name == "ntfy-send" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("attempts = %d", count)
	}
}

func TestStaleRemoteClientIsNotAnAttachedView(t *testing.T) {
	workspace := &Workspace{Attachments: map[string]*Attachment{
		"remote": {
			ID: "remote", Transport: Transport{Kind: "ssh"}, Status: AttachmentReady,
			Views:            map[string]RuntimeView{"pane": {PaneID: "pane", WindowID: 1, Ready: true}},
			ClientHeartbeats: map[string]time.Time{"pane": time.Now().UTC().Add(-10 * time.Second)},
		},
	}}
	if paneAttached(workspace, "pane") {
		t.Fatal("stale SSH client counted as attached")
	}
	workspace.Attachments["remote"].ClientHeartbeats["pane"] = time.Now().UTC()
	if !paneAttached(workspace, "pane") {
		t.Fatal("fresh SSH client was not counted as attached")
	}
}

func TestWaitJoinsAsynchronousTransition(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{}, 1)
	runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
			return "[]", "", nil
		}
		if name == "ntfy-send" {
			close(started)
			<-release
		}
		return "", "", nil
	}}
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
	})
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	code := 1
	if _, err := d.applyEvent(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_exit", Source: "pane-host", ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("transition did not start")
	}
	waited := make(chan struct{})
	go func() { _ = d.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned early")
	case <-time.After(20 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait did not join worker")
	}
}

func TestStatePriorityAndTitleMarker(t *testing.T) {
	if statePriority(StateError) <= statePriority(StateBlocked) || statePriority(StateDone) <= statePriority(StateWorking) {
		t.Fatal("state priority")
	}
	if got := stripStateMarker("[!] agents"); got != "agents" {
		t.Fatalf("strip = %q", got)
	}
}
