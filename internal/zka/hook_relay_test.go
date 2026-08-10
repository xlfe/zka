package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

type recordingHookRelayUpstream struct {
	mu     sync.Mutex
	events []Event
	seen   chan Event
}

func newRecordingHookRelayUpstream() *recordingHookRelayUpstream {
	return &recordingHookRelayUpstream{seen: make(chan Event, 256)}
}

func (u *recordingHookRelayUpstream) Event(_ context.Context, event Event) (*Workspace, error) {
	u.mu.Lock()
	u.events = append(u.events, event)
	u.mu.Unlock()
	u.seen <- event
	return &Workspace{}, nil
}

func (u *recordingHookRelayUpstream) Events() []Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Event(nil), u.events...)
}

func startTestHookRelay(t *testing.T, upstream hookRelayUpstream) (*hookRelayServer, string) {
	t.Helper()
	path := filepath.Join(testRoot(t), "relay.sock")
	listener, err := listenUnixExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	server := newHookRelayServer(listener, upstream, "trusted-workspace", "trusted-pane", log.New(io.Discard, "", 0), nil)
	server.Start()
	t.Cleanup(func() { server.Stop(time.Second) })
	return server, path
}

func writeHookRelayPayload(t *testing.T, path string, payload []byte) {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		if err := unixConn.CloseWrite(); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	_ = conn.Close()
}

func receiveRelayEvent(t *testing.T, upstream *recordingHookRelayUpstream) Event {
	t.Helper()
	select {
	case event := <-upstream.seen:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("relay event was not delivered")
		return Event{}
	}
}

func TestHookRelayPreservesAcceptOrderAcrossConcurrentParsers(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	server, path := startTestHookRelay(t, upstream)

	first, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := io.WriteString(first, `{"version":1,"agent":"claude","kind":"permission_request","turn_id":"one"`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.sequence.Load() >= 1 })

	writeHookRelayPayload(t, path, []byte(`{"version":1,"agent":"claude","kind":"post_tool","turn_id":"two"}`))
	if _, err := io.WriteString(first, "}"); err != nil {
		t.Fatal(err)
	}
	if unixConn, ok := first.(*net.UnixConn); ok {
		if err := unixConn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}

	if got := receiveRelayEvent(t, upstream); got.Kind != "permission_request" || got.TurnID != "one" {
		t.Fatalf("first event = %#v", got)
	}
	if got := receiveRelayEvent(t, upstream); got.Kind != "post_tool" || got.TurnID != "two" {
		t.Fatalf("second event = %#v", got)
	}
}

type forwardingGateUpstream struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	once    sync.Once
	events  []Event
	target  hookRelayUpstream
}

func (u *forwardingGateUpstream) Event(ctx context.Context, event Event) (*Workspace, error) {
	u.once.Do(func() { close(u.started) })
	select {
	case <-u.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u.mu.Lock()
	u.events = append(u.events, event)
	u.mu.Unlock()
	return u.target.Event(ctx, event)
}

func (u *forwardingGateUpstream) Kinds() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	kinds := make([]string, len(u.events))
	for i := range u.events {
		kinds[i] = u.events[i].Kind
	}
	return kinds
}

func TestHookRelayFIFOEndsInLastAcceptedStateUnderSaturation(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	gate := &forwardingGateUpstream{
		started: make(chan struct{}), release: make(chan struct{}),
		target: hookRelayAPI{upstream: daemonHookRelayUpstream{d}, workspaceID: workspace.ID, paneID: pane.ID},
	}
	stats := &hookRelayStats{}
	scheduler := newHookRelayScheduler(gate, log.New(io.Discard, "", 0), stats)

	scheduler.Submit(Event{Kind: "session_start", Source: "claude-hook", TurnID: "start"})
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("upstream call did not start")
	}
	for i, kind := range []string{
		"permission_request", "post_tool", "user_prompt", "permission_request",
		"post_tool", "stop", "permission_request", "post_tool",
	} {
		scheduler.Submit(Event{Kind: kind, Source: "claude-hook", TurnID: string(rune('a' + i))})
	}
	// The queue is full. This terminal post_tool evicts the oldest queued
	// noncritical event without moving any retained event around it.
	scheduler.Submit(Event{Kind: "post_tool", Source: "claude-hook", TurnID: "last"})
	close(gate.release)
	scheduler.Stop(2 * time.Second)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Fatal(err)
	}

	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	reported := got.Panes[pane.ID]
	if reported.State != StateWorking || reported.LastTurnID != "last" || reported.Evidence.Event != "post_tool" {
		t.Fatalf("final pane = %#v; delivered %v", reported, gate.Kinds())
	}
	// Notification workers may legitimately observe an emitted intermediate
	// attention state. They must never record a state/turn pair the accepted
	// sequence did not contain.
	allowedNotifications := map[string]bool{
		"ntfy:blocked:a": true, "ntfy:blocked:d": true,
		"ntfy:done:f": true, "ntfy:blocked:g": true,
	}
	for key := range reported.Notifications {
		if !allowedNotifications[key] {
			t.Fatalf("notification for a non-emitted state: %s; records=%#v", key, reported.Notifications)
		}
	}
	if stats.evicted.Load() != 1 {
		t.Fatalf("queue evictions = %d, want 1", stats.evicted.Load())
	}
}

type daemonHookRelayUpstream struct{ daemon *Daemon }

func (u daemonHookRelayUpstream) Event(ctx context.Context, event Event) (*Workspace, error) {
	return u.daemon.applyEvent(ctx, event)
}

func TestHookRelayQueueEvictsOldestCriticalOnlyWhenNecessary(t *testing.T) {
	gate := &blockingRecordingUpstream{started: make(chan struct{}), release: make(chan struct{})}
	stats := &hookRelayStats{}
	scheduler := newHookRelayScheduler(gate, log.New(io.Discard, "", 0), stats)
	scheduler.Submit(Event{Kind: "session_start", TurnID: "in-flight"})
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("upstream call did not start")
	}
	for i := 0; i < hookRelayQueueSize; i++ {
		scheduler.Submit(Event{Kind: "permission_request", TurnID: string(rune('a' + i))})
	}
	scheduler.Submit(Event{Kind: "stop", TurnID: "last"})
	close(gate.release)
	scheduler.Stop(2 * time.Second)
	events := gate.Events()
	if len(events) != hookRelayQueueSize+1 {
		t.Fatalf("delivered %d events: %#v", len(events), events)
	}
	if events[1].TurnID != "b" || events[len(events)-1].TurnID != "last" {
		t.Fatalf("critical FIFO = %#v", events)
	}
}

type blockingRecordingUpstream struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	once    sync.Once
	events  []Event
}

func (u *blockingRecordingUpstream) Event(ctx context.Context, event Event) (*Workspace, error) {
	u.once.Do(func() { close(u.started) })
	select {
	case <-u.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u.mu.Lock()
	u.events = append(u.events, event)
	u.mu.Unlock()
	return &Workspace{}, nil
}

func (u *blockingRecordingUpstream) Events() []Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Event(nil), u.events...)
}

func TestHookRelayRejectsAuthorityExpansionAndSanitizesDetail(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	server, path := startTestHookRelay(t, upstream)
	invalid := []string{
		`{"version":1,"agent":"codex","kind":"stop","workspace_id":"forged"}`,
		`{"version":1,"agent":"codex","kind":"stop","pane_id":"forged"}`,
		`{"version":1,"agent":"codex","kind":"stop","source":"forged"}`,
		`{"version":1,"agent":"codex","kind":"stop","op":"kill_workspace"}`,
		`{"version":1,"agent":"codex","kind":"stop","pid":42}`,
		`{"version":1,"agent":"codex","kind":"stop","exit_code":1}`,
		`{"version":1,"agent":"codex","kind":"stop","fields":{"backend":"dead"}}`,
		`{"version":1,"agent":"codex","kind":"kill_workspace"}`,
		`{"version":1,"agent":"other","kind":"stop"}`,
		`{"version":2,"agent":"codex","kind":"stop"}`,
		`{"version":1,"agent":"codex","kind":"stop","turn_id":"bad\u001b"}`,
		`{"version":1,"agent":"codex","kind":"stop"}{}`,
		`not json`,
		`{"version":1,"agent":"codex","kind":"stop","turn_id":"` + strings.Repeat("x", hookRelayMaxTurnID+1) + `"}`,
	}
	for _, payload := range invalid {
		writeHookRelayPayload(t, path, []byte(payload))
	}
	writeHookRelayPayload(t, path, bytes.Repeat([]byte("x"), hookRelayMaxMessage+1))
	waitFor(t, func() bool {
		return server.sequence.Load() >= uint64(len(invalid)+1) && len(server.parserSlots) == 0
	})
	writeHookRelayPayload(t, path, []byte(`{"version":1,"agent":"claude","kind":"permission_request","turn_id":"turn","detail":"before\u001b[2J\u0085 after\nline"}`))

	got := receiveRelayEvent(t, upstream)
	if got.WorkspaceID != "trusted-workspace" || got.PaneID != "trusted-pane" || got.Source != "claude-hook" {
		t.Fatalf("trusted fields = %#v", got)
	}
	if got.Detail != "before[2J afterline" {
		t.Fatalf("sanitized detail = %q", got.Detail)
	}
	time.Sleep(150 * time.Millisecond)
	if events := upstream.Events(); len(events) != 1 {
		t.Fatalf("rejected requests reached upstream: %#v", events)
	}
}

func TestHookRelayParserSaturationDropsAndRecovers(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	server, path := startTestHookRelay(t, upstream)
	blocked := make([]net.Conn, 0, hookRelayMaxParsers)
	for i := 0; i < hookRelayMaxParsers; i++ {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "{"); err != nil {
			t.Fatal(err)
		}
		blocked = append(blocked, conn)
	}
	waitFor(t, func() bool { return server.sequence.Load() >= hookRelayMaxParsers })
	dropped, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(dropped, `{"version":1,"agent":"codex","kind":"stop","turn_id":"dropped"}`)
	_ = dropped.Close()
	for _, conn := range blocked {
		_ = conn.Close()
	}
	waitFor(t, func() bool { return len(server.parserSlots) == 0 })
	writeHookRelayPayload(t, path, []byte(`{"version":1,"agent":"codex","kind":"stop","turn_id":"accepted"}`))
	waitFor(t, func() bool {
		events := upstream.Events()
		return len(events) != 0 && events[len(events)-1].TurnID == "accepted"
	})
	if events := upstream.Events(); len(events) > 2 || events[len(events)-1].TurnID != "accepted" {
		t.Fatalf("parser did not recover in order: %#v", events)
	}
}

func TestHookRelayDetailTruncationPreservesUTF8(t *testing.T) {
	got := sanitizeHookRelayDetail(strings.Repeat("é", 100))
	if !utf8.ValidString(got) || len(got) > hookRelayMaxDetail || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated detail = %q (%d bytes)", got, len(got))
	}
}

func TestHookRelayTokenBucketIsBoundedAndRefills(t *testing.T) {
	now := time.Unix(100, 0)
	bucket := newHookRelayBucket(2, 3, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if !bucket.Allow() {
			t.Fatalf("burst token %d rejected", i)
		}
	}
	if bucket.Allow() {
		t.Fatal("bucket exceeded burst")
	}
	now = now.Add(500 * time.Millisecond)
	if !bucket.Allow() || bucket.Allow() {
		t.Fatal("bucket did not refill exactly one token")
	}
}

func TestHookRelayCriticalBucketIsReservedAndBounded(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	path := filepath.Join(testRoot(t), "relay.sock")
	listener, err := listenUnixExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(time.Unix(100, 0).UnixNano())
	server := newHookRelayServer(
		listener, upstream, "trusted-workspace", "trusted-pane",
		log.New(io.Discard, "", 0), func() time.Time { return time.Unix(0, clock.Load()) },
	)
	server.Start()
	t.Cleanup(func() { server.Stop(time.Second) })

	sent := 0
	send := func(payload string) {
		t.Helper()
		sent++
		writeHookRelayPayload(t, path, []byte(payload))
		waitFor(t, func() bool {
			outcomes := len(upstream.Events()) +
				int(server.stats.noncriticalDropped.Load()) +
				int(server.stats.criticalDropped.Load())
			return outcomes == sent
		})
	}
	for i := 0; i < 12; i++ {
		send(`{"version":1,"agent":"claude","kind":"post_tool"}`)
	}
	send(`{"version":1,"agent":"claude","kind":"permission_request","turn_id":"critical"}`)
	events := upstream.Events()
	last := events[len(events)-1]
	if last.Kind != "permission_request" || last.TurnID != "critical" {
		t.Fatalf("critical event after exhausted bucket = %#v; delivered=%d", last, len(events))
	}
	for i := 0; i < 25; i++ {
		kind := "permission_request"
		if i%2 != 0 {
			kind = "stop"
		}
		send(`{"version":1,"agent":"claude","kind":"` + kind + `"}`)
	}
	if got := countHookRelayCritical(upstream.Events()); got != int(hookRelayCriticalBurst) {
		t.Fatalf("critical burst delivered %d events, want %d", got, int(hookRelayCriticalBurst))
	}
	clock.Add(int64(time.Second))
	for i := 0; i < 25; i++ {
		kind := "permission_request"
		if i%2 != 0 {
			kind = "stop"
		}
		send(`{"version":1,"agent":"claude","kind":"` + kind + `"}`)
	}
	if got := countHookRelayCritical(upstream.Events()); got != 2*int(hookRelayCriticalBurst) {
		t.Fatalf("critical rate delivered %d events over one second, want %d", got, 2*int(hookRelayCriticalBurst))
	}
	if dropped := server.stats.criticalDropped.Load(); dropped != 31 {
		t.Fatalf("critical limiter dropped %d events, want 31", dropped)
	}
}

func countHookRelayCritical(events []Event) int {
	count := 0
	for _, event := range events {
		if hookRelayCritical(event.Kind) {
			count++
		}
	}
	return count
}

type transientAcceptListener struct {
	hookRelayListener
	attempted chan struct{}
	once      sync.Once
	err       error
}

func (l *transientAcceptListener) Accept() (net.Conn, error) {
	injected := false
	l.once.Do(func() {
		injected = true
		close(l.attempted)
	})
	if injected {
		return nil, l.err
	}
	return l.hookRelayListener.Accept()
}

func TestHookRelayRetriesTransientAcceptError(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	path := filepath.Join(testRoot(t), "relay.sock")
	owned, err := listenUnixExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	listener := &transientAcceptListener{
		hookRelayListener: owned,
		attempted:         make(chan struct{}),
		err:               &net.OpError{Op: "accept", Net: "unix", Err: syscall.EMFILE},
	}
	var journal syncBuffer
	server := newHookRelayServer(listener, upstream, "workspace", "pane", log.New(&journal, "", 0), nil)
	server.Start()
	t.Cleanup(func() { server.Stop(time.Second) })
	select {
	case <-listener.attempted:
	case <-time.After(time.Second):
		t.Fatal("transient accept error was not injected")
	}
	writeHookRelayPayload(t, path, []byte(`{"version":1,"agent":"codex","kind":"stop"}`))
	if got := receiveRelayEvent(t, upstream); got.Kind != "stop" {
		t.Fatalf("event after accept retry = %#v", got)
	}
	if !strings.Contains(journal.String(), "temporary accept failure") {
		t.Fatalf("accept retry was not logged: %q", journal.String())
	}
	select {
	case err := <-server.fatal:
		t.Fatalf("transient accept error became fatal: %v", err)
	default:
	}
}

func TestRelayHookTakesPrecedenceOverIdentityEnvironment(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	_, path := startTestHookRelay(t, upstream)
	t.Setenv("ZKA_HOOK_SOCKET", path)
	t.Setenv("ZKA_WORKSPACE_ID", "forged-workspace")
	t.Setenv("ZKA_PANE_ID", "forged-pane")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("ZKA_RUNTIME_DIR", "")
	var output, stderr bytes.Buffer
	code, err := Run([]string{"hook", "codex"}, strings.NewReader(`{"hook_event_name":"Stop","turn_id":"turn-1"}`), &output, &stderr)
	if err != nil || code != 0 || output.String() != "{}\n" || stderr.Len() != 0 {
		t.Fatalf("hook = %d, %v, %q, %q", code, err, output.String(), stderr.String())
	}
	got := receiveRelayEvent(t, upstream)
	if got.WorkspaceID != "trusted-workspace" || got.PaneID != "trusted-pane" || got.Kind != "stop" || got.Source != "codex-hook" {
		t.Fatalf("relayed hook = %#v", got)
	}
}

func TestRelayHookRunNeedsNeitherIdentityNorRuntimePaths(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	_, path := startTestHookRelay(t, upstream)
	t.Setenv("ZKA_HOOK_SOCKET", path)
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("ZKA_RUNTIME_DIR", "")
	var output, stderr bytes.Buffer
	code, err := Run([]string{"hook", "claude"}, strings.NewReader(`{"hook_event_name":"Stop","prompt_id":"turn-1"}`), &output, &stderr)
	if err != nil || code != 0 || output.String() != "{}\n" || stderr.Len() != 0 {
		t.Fatalf("hook = %d, %v, %q, %q", code, err, output.String(), stderr.String())
	}
	if got := receiveRelayEvent(t, upstream); got.Kind != "stop" || got.TurnID != "turn-1" || got.Source != "claude-hook" {
		t.Fatalf("relayed hook = %#v", got)
	}
}

func TestRelayHookOmitsInvalidTurnIDWithoutDroppingEvent(t *testing.T) {
	upstream := newRecordingHookRelayUpstream()
	_, path := startTestHookRelay(t, upstream)
	t.Setenv("ZKA_HOOK_SOCKET", path)
	for _, input := range []string{
		`{"hook_event_name":"Stop","turn_id":"` + strings.Repeat("x", hookRelayMaxTurnID+1) + `"}`,
		`{"hook_event_name":"Stop","turn_id":"bad\u001b"}`,
	} {
		var output bytes.Buffer
		code, err := runHook([]string{"codex"}, Paths{}, strings.NewReader(input), &output)
		if err != nil || code != 0 || output.String() != "{}\n" {
			t.Fatalf("hook = %d, %v, %q", code, err, output.String())
		}
		if got := receiveRelayEvent(t, upstream); got.Kind != "stop" || got.TurnID != "" {
			t.Fatalf("event with invalid turn ID = %#v", got)
		}
	}
}

func TestRelayedCodexAndClaudeWithoutIdentityTouchOnlyBoundPane(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	listener, err := listenUnixExclusive(filepath.Join(testRoot(t), "relay.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := newHookRelayServer(listener, daemonHookRelayUpstream{d}, workspace.ID, panes[0].ID, log.New(io.Discard, "", 0), nil)
	server.Start()
	t.Cleanup(func() { server.Stop(time.Second) })
	t.Setenv("ZKA_HOOK_SOCKET", listener.path)
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")

	for _, test := range []struct {
		agent, input string
		want         AgentState
	}{
		{"codex", `{"hook_event_name":"UserPromptSubmit","turn_id":"codex-turn"}`, StateWorking},
		{"claude", `{"hook_event_name":"PermissionRequest","prompt_id":"claude-turn","tool_name":"Bash"}`, StateBlocked},
	} {
		var output bytes.Buffer
		code, err := runHook([]string{test.agent}, Paths{}, strings.NewReader(test.input), &output)
		if err != nil || code != 0 || output.String() != "{}\n" {
			t.Fatalf("%s hook = %d, %v, %q", test.agent, code, err, output.String())
		}
		waitFor(t, func() bool {
			got, getErr := d.getWorkspace(workspace.ID)
			return getErr == nil && got.Panes[panes[0].ID].State == test.want
		})
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound := got.Panes[panes[0].ID]; bound.Agent != "claude" || bound.LastTurnID != "claude-turn" || bound.Evidence.Source != "claude-hook" {
		t.Fatalf("bound pane = %#v", bound)
	}
	if other := got.Panes[panes[1].ID]; other.State != StateUnknown || other.Agent != "" || other.Evidence.Event != "pane_created" || other.Evidence.Source != "zka" {
		t.Fatalf("unbound pane changed = %#v", other)
	}
}

func TestHookRelayMissingDaemonRemainsBestEffort(t *testing.T) {
	root := testRoot(t)
	path := filepath.Join(root, "relay.sock")
	listener, err := listenUnixExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	var journal syncBuffer
	server := newHookRelayServer(listener, NewAPI(testPaths(root)), "workspace", "pane", log.New(&journal, "", 0), nil)
	server.Start()
	t.Cleanup(func() { server.Stop(time.Second) })
	t.Setenv("ZKA_HOOK_SOCKET", path)
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")
	var output bytes.Buffer
	code, err := runHook([]string{"codex"}, Paths{}, strings.NewReader(`{"hook_event_name":"Stop"}`), &output)
	if err != nil || code != 0 || output.String() != "{}\n" {
		t.Fatalf("hook = %d, %v, %q", code, err, output.String())
	}
	waitFor(t, func() bool { return strings.Contains(journal.String(), "upstream delivery unavailable") })
}

func TestHookRelaySessionPermissionsLivenessDoctorAndSweep(t *testing.T) {
	paths := testPaths(testRoot(t))
	logger := log.New(io.Discard, "", 0)
	session, err := createHookRelaySession(paths, logger)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			session.Close()
		}
	})
	for path, want := range map[string]os.FileMode{
		hookRelayRoot(paths): 0o700,
		session.dir:          0o700,
		session.socket:       0o600,
		filepath.Join(session.dir, "session.lock"): 0o600,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	old := time.Now().Add(-2 * hookRelayStaleGrace)
	if err := os.Chtimes(session.dir, old, old); err != nil {
		t.Fatal(err)
	}
	sweepHookRelaySessions(hookRelayRoot(paths), time.Now(), logger)
	if _, err := os.Stat(session.dir); err != nil {
		t.Fatalf("active session swept: %v", err)
	}
	if check := hookRelayDoctorCheck(paths); !check.OK || check.Warning || !strings.Contains(check.Detail, "1 active") {
		t.Fatalf("active doctor check = %#v", check)
	}
	if err := session.listener.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := session.listener.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("doctor connected to the hook relay")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("probe-free accept = %v", err)
	}
	_ = session.listener.SetDeadline(time.Time{})

	// Simulate SIGKILL: the kernel closes the listener and releases flock, but
	// no cleanup code gets an opportunity to remove either filesystem path.
	if err := session.listener.UnixListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(session.lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := session.lock.Close(); err != nil {
		t.Fatal(err)
	}
	session.listener, session.lock = nil, nil
	if check := hookRelayDoctorCheck(paths); !check.OK || !check.Warning || !strings.Contains(check.Detail, "1 stale") {
		t.Fatalf("stale doctor check = %#v", check)
	}
	sweepHookRelaySessions(hookRelayRoot(paths), time.Now(), logger)
	cleaned = true
	if _, err := os.Stat(session.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale session remains: %v", err)
	}
	if _, err := os.Stat(session.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket remains: %v", err)
	}
}

func TestHookRelaySweepPreservesReplacementSocketInode(t *testing.T) {
	paths := testPaths(testRoot(t))
	logger := log.New(io.Discard, "", 0)
	session, err := createHookRelaySession(paths, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Close()
		_ = os.Remove(session.socket)
	})
	if err := session.listener.UnixListener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(session.lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := session.lock.Close(); err != nil {
		t.Fatal(err)
	}
	session.listener, session.lock = nil, nil
	if err := os.Remove(session.socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session.socket, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Model immediate inode reuse deterministically. A stale ownership record
	// can have the same device/inode pair as a replacement regular file, but
	// that must never make the replacement eligible for socket cleanup.
	info, err := os.Lstat(session.socket)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("inspect replacement inode")
	}
	record := hookRelaySocketRecord{
		Version: hookRelayProtocolVersion,
		Device:  uint64(stat.Dev),
		Inode:   uint64(stat.Ino),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.dir, "socket.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * hookRelayStaleGrace)
	if err := os.Chtimes(session.dir, old, old); err != nil {
		t.Fatal(err)
	}
	sweepHookRelaySessions(hookRelayRoot(paths), time.Now(), logger)
	if raw, err := os.ReadFile(session.socket); err != nil || string(raw) != "replacement" {
		t.Fatalf("replacement socket path changed: %q, %v", raw, err)
	}
	if _, err := os.Stat(session.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale session metadata remains after mismatch: %v", err)
	}
}

func TestHookRelaySweepRemovesUnverifiableSessionMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{"missing record", func(path string) error { return os.Remove(path) }},
		{"truncated record", func(path string) error { return os.WriteFile(path, []byte("{"), 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := testPaths(testRoot(t))
			var journal syncBuffer
			logger := log.New(&journal, "", 0)
			session, err := createHookRelaySession(paths, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				session.Close()
				_ = os.Remove(session.socket)
			})
			if err := session.listener.UnixListener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(session.lock.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if err := session.lock.Close(); err != nil {
				t.Fatal(err)
			}
			session.listener, session.lock = nil, nil
			if err := test.mutate(filepath.Join(session.dir, "socket.json")); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * hookRelayStaleGrace)
			if err := os.Chtimes(session.dir, old, old); err != nil {
				t.Fatal(err)
			}
			sweepHookRelaySessions(hookRelayRoot(paths), time.Now(), logger)
			if _, err := os.Stat(session.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverifiable session metadata remains: %v", err)
			}
			if info, err := os.Lstat(session.socket); err != nil || info.Mode()&os.ModeSocket == 0 {
				t.Fatalf("unproven socket inode was removed: %#v, %v", info, err)
			}
			before := journal.String()
			sweepHookRelaySessions(hookRelayRoot(paths), time.Now(), logger)
			if after := journal.String(); after != before {
				t.Fatalf("removed session was reported again: before=%q after=%q", before, after)
			}
		})
	}
}

func TestHookRelaySessionLockRetriesDoctorRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	owner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	contender, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	time.AfterFunc(hookRelayLockRetryDelay/2, func() {
		_ = syscall.Flock(int(owner.Fd()), syscall.LOCK_UN)
		close(released)
	})
	if err := lockHookRelaySession(contender); err != nil {
		t.Fatalf("relay did not survive transient doctor lock: %v", err)
	}
	<-released
	_ = syscall.Flock(int(contender.Fd()), syscall.LOCK_UN)
}

func TestHookRelayDoctorTreatsYoungIncompleteSessionAsStarting(t *testing.T) {
	paths := testPaths(testRoot(t))
	root := hookRelayRoot(paths)
	if err := prepareHookRelayRoot(root); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, strings.Repeat("a", 32))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if check := hookRelayDoctorCheck(paths); !check.OK || !strings.Contains(check.Detail, "1 starting") {
		t.Fatalf("directory-only startup check = %#v", check)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	if check := hookRelayDoctorCheck(paths); !check.OK || !strings.Contains(check.Detail, "1 starting") {
		t.Fatalf("unlocked startup check = %#v", check)
	}
}

func TestHookRelaySupervisorChildProcess(t *testing.T) {
	if os.Getenv("ZKA_TEST_HOOK_RELAY_CHILD") != "1" {
		return
	}
	values := map[string]string{}
	for _, name := range []string{
		"ZKA_HOOK_RELAY_SOCKET", "ZKA_WORKSPACE_ID", "ZKA_PANE_ID", "ZKA_SOCKET", "KITTY_LISTEN_ON",
	} {
		values[name] = os.Getenv(name)
	}
	info, err := os.Lstat(values["ZKA_HOOK_RELAY_SOCKET"])
	if err == nil {
		values["relay_mode"] = info.Mode().Perm().String()
	}
	encoded, _ := json.Marshal(values)
	_ = os.WriteFile(os.Getenv("ZKA_TEST_HOOK_RELAY_RESULT"), encoded, 0o600)
	os.Exit(17)
}

func TestRelayCommandDefaultsIdentityPreservesLauncherEnvironmentAndReaps(t *testing.T) {
	// The relay binds under this root, so it has to clear the real sockaddr_un
	// ceiling rather than merely exist.
	root := testRoot(t)
	resultPath := filepath.Join(root, "child.json")
	controlSocket := filepath.Join(root, "control.sock")
	t.Setenv("ZKA_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("ZKA_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("ZKA_SOCKET", controlSocket)
	t.Setenv("ZKA_WORKSPACE_ID", "trusted-workspace")
	t.Setenv("ZKA_PANE_ID", "trusted-pane")
	t.Setenv("KITTY_LISTEN_ON", "unix:/host/kitty.sock")
	t.Setenv("ZKA_TEST_HOOK_RELAY_CHILD", "1")
	t.Setenv("ZKA_TEST_HOOK_RELAY_RESULT", resultPath)

	var stdout, stderr bytes.Buffer
	code, err := Run([]string{"relay", "hooks", "--", os.Args[0], "-test.run=^TestHookRelaySupervisorChildProcess$"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil || code != 17 {
		t.Fatalf("relay = %d, %v; stderr=%s", code, err, stderr.String())
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values["ZKA_WORKSPACE_ID"] != "trusted-workspace" || values["ZKA_PANE_ID"] != "trusted-pane" ||
		values["ZKA_SOCKET"] != controlSocket || values["KITTY_LISTEN_ON"] != "unix:/host/kitty.sock" {
		t.Fatalf("launcher environment = %#v", values)
	}
	relaySocket := values["ZKA_HOOK_RELAY_SOCKET"]
	if relaySocket == "" || values["relay_mode"] != "-rw-------" {
		t.Fatalf("relay capability = %#v", values)
	}
	if _, err := os.Stat(relaySocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay socket remains after child reap: %v", err)
	}
}

func TestRelayCommandStartsOutsideManagedPaneWithExplicitIdentity(t *testing.T) {
	root := testRoot(t)
	resultPath := filepath.Join(root, "child.json")
	t.Setenv("ZKA_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("ZKA_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("ZKA_SOCKET", filepath.Join(root, "zka.sock"))
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")
	t.Setenv("ZKA_TEST_HOOK_RELAY_CHILD", "1")
	t.Setenv("ZKA_TEST_HOOK_RELAY_RESULT", resultPath)

	var stdout, stderr bytes.Buffer
	code, err := Run([]string{
		"relay", "hooks", "--workspace", "trusted-workspace", "--pane", "trusted-pane", "--",
		os.Args[0], "-test.run=^TestHookRelaySupervisorChildProcess$",
	}, strings.NewReader(""), &stdout, &stderr)
	if err != nil || code != 17 {
		t.Fatalf("relay = %d, %v; stderr=%s", code, err, stderr.String())
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values["ZKA_HOOK_RELAY_SOCKET"] == "" || values["ZKA_WORKSPACE_ID"] != "" || values["ZKA_PANE_ID"] != "" {
		t.Fatalf("launcher environment = %#v", values)
	}
}

func TestHookRelayBlockingChildProcess(t *testing.T) {
	if os.Getenv("ZKA_TEST_HOOK_RELAY_BLOCKING_CHILD") != "1" {
		return
	}
	ready := os.Getenv("ZKA_TEST_HOOK_RELAY_READY")
	var interrupts chan os.Signal
	if os.Getenv("ZKA_TEST_HOOK_RELAY_GRACEFUL_INT") == "1" {
		interrupts = make(chan os.Signal, 2)
		signal.Notify(interrupts, os.Interrupt)
	}
	payload := os.Getenv("ZKA_HOOK_RELAY_SOCKET") + "\n" + strconv.Itoa(os.Getpid()) + "\n"
	if err := os.WriteFile(ready, []byte(payload), 0o600); err != nil {
		os.Exit(125)
	}
	if interrupts != nil {
		<-interrupts
		select {
		case <-interrupts:
			os.Exit(99)
		case <-time.After(50 * time.Millisecond):
			os.Exit(128 + int(syscall.SIGINT))
		}
	}
	select {}
}

func TestHookRelaySupervisorProcess(t *testing.T) {
	if os.Getenv("ZKA_TEST_HOOK_RELAY_SUPERVISOR") != "1" {
		return
	}
	code, err := Run(
		[]string{"relay", "hooks", "--", os.Args[0], "-test.run=^TestHookRelayBlockingChildProcess$"},
		os.Stdin, os.Stdout, os.Stderr,
	)
	if err != nil {
		os.Exit(125)
	}
	os.Exit(code)
}

func TestHookRelaySupervisorCleansUpOnSignals(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			root := testRoot(t)
			ready := filepath.Join(root, "ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHookRelaySupervisorProcess$")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Env = append(os.Environ(),
				"ZKA_TEST_HOOK_RELAY_SUPERVISOR=1",
				"ZKA_TEST_HOOK_RELAY_BLOCKING_CHILD=1",
				"ZKA_TEST_HOOK_RELAY_READY="+ready,
				"ZKA_TEST_HOOK_RELAY_GRACEFUL_INT=1",
				"ZKA_RUNTIME_DIR="+filepath.Join(root, "run"),
				"ZKA_STATE_DIR="+filepath.Join(root, "state"),
				"ZKA_SOCKET="+filepath.Join(root, "zka.sock"),
				"ZKA_WORKSPACE_ID=workspace",
				"ZKA_PANE_ID=pane",
			)
			var stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = io.Discard, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { _, statErr := os.Stat(ready); return statErr == nil })
			raw, err := os.ReadFile(ready)
			if err != nil {
				t.Fatal(err)
			}
			readyFields := strings.Fields(string(raw))
			if len(readyFields) != 2 {
				t.Fatalf("invalid child readiness record %q", raw)
			}
			relaySocket := readyFields[0]
			childPID, err := strconv.Atoi(readyFields[1])
			if err != nil {
				t.Fatal(err)
			}
			var signalErr error
			if signal == syscall.SIGINT {
				signalErr = syscall.Kill(-cmd.Process.Pid, signal)
			} else {
				signalErr = cmd.Process.Signal(signal)
			}
			if signalErr != nil {
				t.Fatal(signalErr)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if got := processExitCode(err); got != 128+int(signal) {
					t.Fatalf("exit = %d, want %d; stderr=%s", got, 128+int(signal), stderr.String())
				}
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("relay did not stop after %s", signal)
			}
			if _, err := os.Stat(relaySocket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("relay socket remains after %s: %v", signal, err)
			}
			if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("supervised child %d remains after %s: %v", childPID, signal, err)
			}
		})
	}
}

func TestHookRelayPIDOnlySIGINTDoesNotArmKillTimer(t *testing.T) {
	root := testRoot(t)
	ready := filepath.Join(root, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHookRelaySupervisorProcess$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"ZKA_TEST_HOOK_RELAY_SUPERVISOR=1",
		"ZKA_TEST_HOOK_RELAY_BLOCKING_CHILD=1",
		"ZKA_TEST_HOOK_RELAY_READY="+ready,
		"ZKA_TEST_HOOK_RELAY_GRACEFUL_INT=",
		"ZKA_RUNTIME_DIR="+filepath.Join(root, "run"),
		"ZKA_STATE_DIR="+filepath.Join(root, "state"),
		"ZKA_SOCKET="+filepath.Join(root, "zka.sock"),
		"ZKA_WORKSPACE_ID=workspace",
		"ZKA_PANE_ID=pane",
	)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	waitFor(t, func() bool { _, statErr := os.Stat(ready); return statErr == nil })
	raw, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	readyFields := strings.Fields(string(raw))
	if len(readyFields) != 2 {
		t.Fatalf("invalid child readiness record %q", raw)
	}
	relaySocket := readyFields[0]
	childPID, err := strconv.Atoi(readyFields[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(hookRelayChildStopGrace + 250*time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		finished = true
		t.Fatalf("PID-only SIGINT terminated relay/child: %v; stderr=%s", err, stderr.String())
	case <-timer.C:
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child did not survive ignored PID-only SIGINT: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if got := processExitCode(err); got != 128+int(syscall.SIGTERM) {
			t.Fatalf("cleanup exit = %d; stderr=%s", got, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop after cleanup SIGTERM")
	}
	if _, err := os.Stat(relaySocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay socket remains after cleanup: %v", err)
	}
}

func TestHookRelaySignalPolicyAvoidsDoubleAndCrossSignalEscalation(t *testing.T) {
	policy := hookRelaySignalPolicy{}
	if got := policy.Action(syscall.SIGINT); got != hookRelaySignalIgnore {
		t.Fatalf("SIGINT action = %v", got)
	}
	if got := policy.Action(syscall.SIGQUIT); got != hookRelaySignalIgnore {
		t.Fatalf("SIGQUIT action = %v", got)
	}
	if got := policy.Action(syscall.SIGHUP); got != hookRelaySignalForward {
		t.Fatalf("first SIGHUP action = %v", got)
	}
	if got := policy.Action(syscall.SIGTERM); got != hookRelaySignalForward {
		t.Fatalf("SIGHUP followed by SIGTERM action = %v", got)
	}
	if got := policy.Action(syscall.SIGTERM); got != hookRelaySignalKill {
		t.Fatalf("second SIGTERM action = %v", got)
	}
}

func TestExclusiveUnixListenerRejectsCollision(t *testing.T) {
	path := filepath.Join(testRoot(t), "relay.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixExclusive(path); err == nil {
		t.Fatal("exclusive listener replaced an existing path")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "do not replace" {
		t.Fatalf("collision path changed: %q, %v", raw, err)
	}
}

func TestOwnedUnixListenerDoesNotRemoveReplacementInode(t *testing.T) {
	path := filepath.Join(testRoot(t), "relay.sock")
	listener, err := listenUnixExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "replacement" {
		t.Fatalf("replacement inode changed: %q, %v", raw, err)
	}
}

func TestHookRelayRejectsExcessiveSocketPath(t *testing.T) {
	path := "/" + strings.Repeat("x", safeUnixSocketPath) + ".sock"
	if _, err := listenUnixExclusive(path); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("long socket path error = %v", err)
	}
}
