package zka

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func quietRunner() *fakeRunner {
	return &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
			return "[]", "", nil
		}
		return "", "", nil
	}}
}

func createTestSession(t *testing.T, daemon *Daemon) *Session {
	t.Helper()
	session, err := daemon.createSession(createSessionRequest{Name: "reviewer", BackendKind: "zmx", Agent: "codex", Command: []string{"codex"}, CWD: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestCreateSessionRejectsNonZMXBackend(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.createSession(createSessionRequest{
		Name:        "reviewer",
		BackendKind: "other",
		Command:     []string{"codex"},
		CWD:         "/work",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported backend") {
		t.Fatalf("expected unsupported backend error, got %v", err)
	}
}

func TestDaemonAgentStateTransitions(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	ctx := context.Background()
	tests := []struct {
		kind string
		want AgentState
	}{
		{"session_start", StateIdle},
		{"user_prompt", StateWorking},
		{"permission_request", StateBlocked},
		{"post_tool", StateWorking},
		{"stop", StateDone},
	}
	for _, test := range tests {
		got, err := d.applyEvent(ctx, Event{SessionID: session.ID, Kind: test.kind, Source: "test", TurnID: "turn-1"})
		if err != nil {
			t.Fatalf("%s: %v", test.kind, err)
		}
		if got.State != test.want {
			t.Fatalf("%s state = %s, want %s", test.kind, got.State, test.want)
		}
	}
	seen, err := d.markSeen(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen.State != StateIdle || seen.Evidence.Event != "seen" {
		t.Fatalf("seen result = %#v", seen)
	}
}

func TestStopWhileFocusedIsIdle(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
			return `[{"is_focused":true,"tabs":[{"is_active":true,"windows":[{"id":7,"is_active":true,"user_vars":{"zka_session":"SESSION_ID"}}]}]}]`, "", nil
		}
		return "", "", nil
	}}
	d, err := newTestDaemon(t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	runner.handler = func(_ context.Context, name string, args ...string) (string, string, error) {
		if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
			return strings.ReplaceAll(`[{"is_focused":true,"tabs":[{"is_active":true,"windows":[{"id":7,"is_active":true,"user_vars":{"zka_session":"SESSION_ID"}}]}]}]`, "SESSION_ID", session.ID), "", nil
		}
		return "", "", nil
	}
	_, err = d.registerView(session.ID, View{Endpoint: "unix:/kitty", WindowID: 7, Attached: true, Focused: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "stop", Source: "codex", TurnID: "turn"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, getErr := d.getSession(session.ID)
		return getErr == nil && got.State == StateIdle
	})
}

func TestProcessFailureBecomesError(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	if _, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "process_started", Source: "wrapper", PID: 42}); err != nil {
		t.Fatal(err)
	}
	code := 17
	got, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "process_exit", Source: "wrapper", ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateError || !got.BackendCreated || got.BackendReady {
		t.Fatalf("failed process state = %#v", got)
	}
}

func TestCleanProcessExitPreservesUnseenDone(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	if _, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "process_started", Source: "wrapper", PID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "stop", Source: "codex", TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	code := 0
	got, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "process_exit", Source: "wrapper", ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateDone || got.Process.Running {
		t.Fatalf("clean exit state = %#v", got)
	}
}

func TestDaemonRestartInvalidatesActiveState(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	if _, err := d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "user_prompt", Source: "codex"}); err != nil {
		t.Fatal(err)
	}
	restarted, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUnknown || got.Evidence.Event != "daemon_restart" {
		t.Fatalf("restart state = %#v", got)
	}
}

func TestPrepareViewCreatesOnlyOnce(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	first, err := d.prepareView(session.ID)
	if err != nil || !first.Create {
		t.Fatalf("first prepare = %#v, %v", first, err)
	}
	second, err := d.prepareView(session.ID)
	if err != nil || second.Create {
		t.Fatalf("second prepare = %#v, %v", second, err)
	}
}

func TestRemoveStaleSocketRefusesRegularFile(t *testing.T) {
	path := t.TempDir() + "/socket"
	if err := os.WriteFile(path, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleSocket(path); err == nil {
		t.Fatal("regular file was removed")
	}
}

func TestUnknownEventIsRejected(t *testing.T) {
	d, err := newTestDaemon(t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	session := createTestSession(t, d)
	_, err = d.applyEvent(context.Background(), Event{SessionID: session.ID, Kind: "surprise", Source: "test"})
	if err == nil || !strings.Contains(err.Error(), "unsupported event") {
		t.Fatalf("error = %v", err)
	}
}

func TestListenUnixRejectsActiveListener(t *testing.T) {
	path := testPaths(t.TempDir()).Socket
	first, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, err = listenUnix(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second listener error = %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}
