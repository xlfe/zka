package zka

import (
	"context"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestDesktopNotificationMapsStateToUrgencyAndIcon(t *testing.T) {
	for _, tc := range []struct {
		state   AgentState
		urgency byte
		icon    string
	}{
		{StateBlocked, urgencyCritical, "dialog-question"},
		{StateError, urgencyCritical, "dialog-error"},
		{StateDone, urgencyNormal, "dialog-information"},
		{StateIdle, urgencyNormal, "dialog-information"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			workspace := &Workspace{ID: "ws1", Name: "reviewer"}
			workspace.Origin.Name = "devbox"
			pane := &Pane{ID: "pane1", State: tc.state}
			note := desktopNotification(workspace, pane)
			if note.Urgency != tc.urgency {
				t.Errorf("urgency = %d, want %d", note.Urgency, tc.urgency)
			}
			if note.Icon != tc.icon {
				t.Errorf("icon = %q, want %q", note.Icon, tc.icon)
			}
			if want := notificationTitle(workspace, pane); note.Summary != want {
				t.Errorf("summary = %q, want %q", note.Summary, want)
			}
			if want := notificationBody(workspace, pane, true); note.Body != want {
				t.Errorf("body = %q, want %q", note.Body, want)
			}
			if note.WorkspaceID != "ws1" || note.PaneID != "pane1" {
				t.Errorf("identity = %s/%s", note.WorkspaceID, note.PaneID)
			}
		})
	}
}

// Anything the user must act on stays until it is withdrawn; everything else
// follows the server default. A wrong value here silently drops a blocked
// notification off the screen before it is seen.
func TestDesktopExpireTimeoutKeepsCriticalUntilWithdrawn(t *testing.T) {
	if got := desktopExpireTimeout(urgencyCritical); got != 0 {
		t.Errorf("critical expire = %d, want 0", got)
	}
	if got := desktopExpireTimeout(urgencyNormal); got != -1 {
		t.Errorf("normal expire = %d, want -1", got)
	}
}

// godbus infers the D-Bus signature from the concrete Go types, so a wrong type
// here is a runtime InvalidArgs that would only ever appear against a live bus.
func TestNotifyArgsUseWireTypes(t *testing.T) {
	note := DesktopNotification{
		Summary: "s", Body: "b", Urgency: urgencyCritical,
		Icon: "dialog-question", ActionLabel: "Focus",
	}
	args := notifyArgs(note, 41, false, true)
	want := []reflect.Type{
		reflect.TypeOf(""),
		reflect.TypeOf(uint32(0)),
		reflect.TypeOf(""),
		reflect.TypeOf(""),
		reflect.TypeOf(""),
		reflect.TypeOf([]string{}),
		reflect.TypeOf(map[string]dbus.Variant{}),
		reflect.TypeOf(int32(0)),
	}
	if len(args) != len(want) {
		t.Fatalf("args = %d, want %d", len(args), len(want))
	}
	for i, expected := range want {
		if got := reflect.TypeOf(args[i]); got != expected {
			t.Errorf("arg %d type = %v, want %v", i, got, expected)
		}
	}
	if args[1].(uint32) != 41 {
		t.Errorf("replaces_id = %v, want 41", args[1])
	}
	hints := args[6].(map[string]dbus.Variant)
	urgency, ok := hints["urgency"]
	if !ok {
		t.Fatal("missing urgency hint")
	}
	if _, ok := urgency.Value().(byte); !ok {
		t.Errorf("urgency hint = %T, want byte", urgency.Value())
	}
	if buttons := args[5].([]string); len(buttons) != 4 || buttons[0] != "default" || buttons[2] != "focus" {
		t.Errorf("actions = %#v", buttons)
	}
}

// A server that does not advertise actions must not be sent a button it cannot
// render or report.
func TestNotifyArgsOmitActionsWhenUnsupported(t *testing.T) {
	note := DesktopNotification{Summary: "s", Body: "b", ActionLabel: "Focus"}
	args := notifyArgs(note, 0, false, false)
	if buttons := args[5].([]string); len(buttons) != 0 {
		t.Errorf("actions = %#v, want none", buttons)
	}
}

// Agent evidence routinely contains & and <, which a body-markup server would
// drop or fail to render.
func TestNotifyArgsEscapeMarkupOnlyWhenAdvertised(t *testing.T) {
	note := DesktopNotification{Summary: "a & b", Body: "<tag>"}
	escaped := notifyArgs(note, 0, true, false)
	if escaped[3].(string) != "a &amp; b" || escaped[4].(string) != "&lt;tag&gt;" {
		t.Errorf("markup server got %q / %q", escaped[3], escaped[4])
	}
	raw := notifyArgs(note, 0, false, false)
	if raw[3].(string) != "a & b" || raw[4].(string) != "<tag>" {
		t.Errorf("plain server got %q / %q", raw[3], raw[4])
	}
}

func TestNotificationRegistryReplacesInPlace(t *testing.T) {
	now := time.Unix(1000, 0)
	registry := newNotificationRegistry()
	pane := paneRef{Workspace: "ws", Pane: "p"}
	if got := registry.replaces(pane); got != 0 {
		t.Fatalf("first replaces = %d, want 0", got)
	}
	registry.commit(pane, 7, now)
	if got := registry.replaces(pane); got != 7 {
		t.Fatalf("second replaces = %d, want 7", got)
	}
	registry.commit(pane, 9, now)
	if got := registry.replaces(pane); got != 9 {
		t.Fatalf("third replaces = %d, want 9", got)
	}
	if _, ok := registry.live[7]; ok {
		t.Error("superseded id still live")
	}
}

// A NotificationClosed for an id that has already been replaced must not evict
// its replacement, or the pane would lose the ability to update in place.
func TestNotificationRegistryIgnoresSupersededClose(t *testing.T) {
	now := time.Unix(1000, 0)
	registry := newNotificationRegistry()
	pane := paneRef{Workspace: "ws", Pane: "p"}
	registry.commit(pane, 7, now)
	registry.commit(pane, 9, now)
	registry.forget(7, now)
	if got := registry.replaces(pane); got != 9 {
		t.Fatalf("replaces after superseded close = %d, want 9", got)
	}
}

// Signals are not ordered across members, so an ActionInvoked can be processed
// after the NotificationClosed for the same id. Without the recent window that
// click is silently dropped and the Focus button does nothing.
func TestNotificationRegistryRoutesActionAfterClose(t *testing.T) {
	now := time.Unix(1000, 0)
	registry := newNotificationRegistry()
	pane := paneRef{Workspace: "ws", Pane: "p"}
	registry.commit(pane, 7, now)
	registry.forget(7, now)
	got, ok := registry.lookup(7, now.Add(time.Second))
	if !ok || got != pane {
		t.Fatalf("lookup after close = %v/%v, want %v/true", got, ok, pane)
	}
	if _, ok := registry.lookup(7, now.Add(2*notificationRecentWindow)); ok {
		t.Error("closed id resolved after the recent window expired")
	}
}

// The match rule receives signals for every notification on the bus, not only
// ours.
func TestNotificationRegistryIgnoresForeignIDs(t *testing.T) {
	registry := newNotificationRegistry()
	if _, ok := registry.lookup(1234, time.Unix(1000, 0)); ok {
		t.Error("resolved an id we never posted")
	}
}

func TestNotificationRegistryTakeRemovesPane(t *testing.T) {
	now := time.Unix(1000, 0)
	registry := newNotificationRegistry()
	pane := paneRef{Workspace: "ws", Pane: "p"}
	if _, ok := registry.take(pane, now); ok {
		t.Error("took an id that was never posted")
	}
	registry.commit(pane, 7, now)
	id, ok := registry.take(pane, now)
	if !ok || id != 7 {
		t.Fatalf("take = %d/%v, want 7/true", id, ok)
	}
	if got := registry.replaces(pane); got != 0 {
		t.Errorf("replaces after take = %d, want 0", got)
	}
}

// A reconnected server issues fresh ids from zero, so stale mappings would route
// a stranger's click into one of our panes.
func TestNotificationRegistryResetDropsMappings(t *testing.T) {
	now := time.Unix(1000, 0)
	registry := newNotificationRegistry()
	pane := paneRef{Workspace: "ws", Pane: "p"}
	registry.commit(pane, 7, now)
	registry.reset()
	if _, ok := registry.lookup(7, now); ok {
		t.Error("id survived a reset")
	}
	if got := registry.replaces(pane); got != 0 {
		t.Errorf("replaces after reset = %d, want 0", got)
	}
}

// The Nix check sandbox has no session bus. godbus would autolaunch dbus-launch
// if asked for a session connection with no address, which must never happen.
func TestDBusNotifierWithoutSessionBusDoesNotDial(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	notifier := newDBusNotifier(log.New(io.Discard, "", 0), nil)
	t.Cleanup(notifier.Shutdown)
	err := notifier.Notify(context.Background(), DesktopNotification{Summary: "s"})
	if err == nil {
		t.Fatal("expected a failure with no session bus")
	}
	if !strings.Contains(err.Error(), "session bus") {
		t.Fatalf("error = %v, want it to name the session bus", err)
	}
	if _, err := notifier.Probe(context.Background()); err == nil {
		t.Fatal("expected probe to fail with no session bus")
	}
}

// TestDBusNotifierAgainstLiveBus exercises the real transport. It is opt-in
// rather than merely session-bus-gated because it puts a notification on
// somebody's screen: set ZKA_LIVE_DBUS_TEST=1 to run it.
func TestDBusNotifierAgainstLiveBus(t *testing.T) {
	if os.Getenv("ZKA_LIVE_DBUS_TEST") == "" {
		t.Skip("set ZKA_LIVE_DBUS_TEST=1 to exercise the session bus")
	}
	actions := make(chan paneRef, 1)
	notifier := newDBusNotifier(log.New(os.Stderr, "", 0), func(workspaceID, paneID string) {
		actions <- paneRef{Workspace: workspaceID, Pane: paneID}
	})
	t.Cleanup(notifier.Shutdown)

	server, err := notifier.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("notification server: %s", server)

	note := DesktopNotification{
		WorkspaceID: "live-ws", PaneID: "live-pane",
		Summary: "zka live test", Body: "posted by go test",
		Urgency: urgencyNormal, Icon: "dialog-information", ActionLabel: "Focus",
	}
	if err := notifier.Notify(context.Background(), note); err != nil {
		t.Fatalf("notify: %v", err)
	}
	// A second send for the same pane must update in place rather than stack.
	if err := notifier.Notify(context.Background(), note); err != nil {
		t.Fatalf("replace: %v", err)
	}
	notifier.Withdraw(context.Background(), note.WorkspaceID, note.PaneID)
}

// Withdraw must not dial: with no connection there is by definition nothing of
// ours still on screen.
func TestDBusNotifierWithdrawWithoutConnectionIsQuiet(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	notifier := newDBusNotifier(log.New(io.Discard, "", 0), nil)
	t.Cleanup(notifier.Shutdown)
	notifier.Withdraw(context.Background(), "ws", "pane")
}
