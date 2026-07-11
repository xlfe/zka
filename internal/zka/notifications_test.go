package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDetachedDoneUsesNtfy(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	session.State = StateDone
	session.LastTurnID = "turn-done"
	session.Evidence = Evidence{Source: "codex", Event: "stop", Timestamp: time.Now().UTC()}
	d.mu.Lock()
	d.state.Sessions[session.ID] = session.Clone()
	if err := d.store.Save(d.state); err != nil {
		t.Fatal(err)
	}
	d.mu.Unlock()
	d.afterTransition(context.Background(), StateWorking, session)
	waitFor(t, func() bool { return hasCommand(runner.Calls(), "ntfy-send") })
	call := firstCommand(runner.Calls(), "ntfy-send")
	joined := strings.Join(call.Args, " ")
	if !strings.Contains(joined, "-p 3") || !strings.Contains(joined, "white_check_mark") {
		t.Fatalf("ntfy args = %#v", call.Args)
	}
}

func TestBlockedUsesHighPriorityNtfy(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	session.State = StateBlocked
	session.LastTurnID = "turn-blocked"
	session.Evidence = Evidence{Source: "codex", Event: "permission_request", Detail: "shell approval", Timestamp: time.Now().UTC()}
	d.mu.Lock()
	d.state.Sessions[session.ID] = session.Clone()
	if err := d.store.Save(d.state); err != nil {
		t.Fatal(err)
	}
	d.mu.Unlock()
	d.afterTransition(context.Background(), StateWorking, session)
	waitFor(t, func() bool { return hasCommand(runner.Calls(), "ntfy-send") })
	call := firstCommand(runner.Calls(), "ntfy-send")
	joined := strings.Join(call.Args, " ")
	if !strings.Contains(joined, "-p 5") || !strings.Contains(joined, "warning") {
		t.Fatalf("ntfy args = %#v", call.Args)
	}
}

func TestNotificationDedupe(t *testing.T) {
	runner := quietRunner()
	d, err := newTestDaemon(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	key := "ntfy:error:event"
	if !d.reserveNotification(session.ID, key, "ntfy") {
		t.Fatal("first reservation failed")
	}
	if d.reserveNotification(session.ID, key, "ntfy") {
		t.Fatal("duplicate reservation succeeded")
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
	d, err := newTestDaemon(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	session.State = StateError
	session.LastTurnID = "turn-error"
	session.Evidence = Evidence{Source: "agent-run", Event: "process_exit", Timestamp: time.Now().UTC()}
	d.mu.Lock()
	d.state.Sessions[session.ID] = session.Clone()
	if err := d.store.Save(d.state); err != nil {
		t.Fatal(err)
	}
	d.mu.Unlock()
	d.afterTransition(context.Background(), StateWorking, session)
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, record := range d.state.Sessions[session.ID].Notifications {
			if record.Channel == "ntfy" && strings.Contains(record.LastError, "token secret is unreadable") {
				return true
			}
		}
		return false
	})
	count := 0
	for _, call := range runner.Calls() {
		if call.Name == "ntfy-send" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("ntfy attempts = %d, want 3", count)
	}
}

func TestStatePriorityAndTitleMarker(t *testing.T) {
	if statePriority(StateError) <= statePriority(StateBlocked) || statePriority(StateDone) <= statePriority(StateWorking) {
		t.Fatal("attention state priority is incorrect")
	}
	if got := stripStateMarker("[!] agents"); got != "agents" {
		t.Fatalf("stripStateMarker = %q", got)
	}
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
