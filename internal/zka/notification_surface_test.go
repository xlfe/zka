package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func deliveryTestState(records map[string]NotificationRecord, state AgentState, turn string) StateData {
	data := StateData{Workspaces: map[string]*Workspace{
		"ws": {
			ID: "ws", Name: "reviewer",
			Panes: map[string]*Pane{
				"pane": {
					ID: "pane", State: state, LastTurnID: turn,
					Evidence:      Evidence{Source: "claude-hook", Event: "permission_request", Timestamp: time.Unix(1000, 0).UTC()},
					Notifications: records,
				},
			},
			Attachments: map[string]*Attachment{},
		},
	}}
	data.Node.ID = "node"
	return data
}

func allChannels() NotificationPolicy {
	return NotificationPolicy{Desktop: true, Ntfy: true}
}

func TestAttentionSnapshotSurfacesDeliveryFailure(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {
			Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 8,
			LastError: "no notification server", Abandoned: true,
		},
	}, StateBlocked, "t1")

	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	if len(snapshot.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(snapshot.Items))
	}
	item := snapshot.Items[0]
	if len(item.Delivery) != 1 || item.Delivery[0].Status != "failed" {
		t.Fatalf("item delivery = %#v", item.Delivery)
	}
	if item.Delivery[0].Channel != "kitty" || item.Delivery[0].Attempts != 8 {
		t.Errorf("delivery detail = %#v", item.Delivery[0])
	}
	if !item.Undelivered() {
		t.Error("item does not report itself undelivered")
	}
	if snapshot.Counts.Undelivered != 1 {
		t.Errorf("counts.undelivered = %d, want 1", snapshot.Counts.Undelivered)
	}
	if snapshot.Delivery.Failed != 1 || !snapshot.Delivery.Broken() {
		t.Errorf("aggregate = %#v", snapshot.Delivery)
	}
	if len(snapshot.Delivery.Channels) != 1 || snapshot.Delivery.Channels[0] != "kitty" {
		t.Errorf("channels = %#v", snapshot.Delivery.Channels)
	}
}

// The property that makes a broken channel unmissable: glancing at the pane must
// not erase the evidence that the user was never told about it.
func TestAttentionDeliveryOutlivesResolvedPane(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {
			Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 8,
			LastError: "no notification server", Abandoned: true,
		},
	}, StateWorking, "t1")

	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	if len(snapshot.Items) != 0 {
		t.Fatalf("a working pane produced items: %#v", snapshot.Items)
	}
	if snapshot.Delivery.Failed != 1 {
		t.Fatalf("aggregate lost the failure once the pane resolved: %#v", snapshot.Delivery)
	}
}

// A failure recorded against an earlier turn is not a failure to report this one.
func TestAttentionSnapshotIgnoresPreviousTurnRecords(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:old": {Key: "kitty:blocked:old", Channel: "kitty", LastError: "stale"},
		"kitty:blocked:t1":  {Key: "kitty:blocked:t1", Channel: "kitty", SentAt: time.Unix(2000, 0).UTC()},
	}, StateBlocked, "t1")

	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	if len(snapshot.Items) != 1 {
		t.Fatalf("items = %d", len(snapshot.Items))
	}
	if snapshot.Items[0].Undelivered() {
		t.Errorf("current item marked undelivered by a previous turn: %#v", snapshot.Items[0].Delivery)
	}
	if snapshot.Counts.Undelivered != 0 {
		t.Errorf("counts.undelivered = %d, want 0", snapshot.Counts.Undelivered)
	}
}

// A disabled channel cannot fail to deliver, so its records must not raise alarm.
func TestAttentionDeliveryIgnoresDisabledChannels(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"ntfy:blocked:t1": {Key: "ntfy:blocked:t1", Channel: "ntfy", LastError: "push refused", Abandoned: true},
	}, StateBlocked, "t1")

	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, NotificationPolicy{Desktop: true})

	if snapshot.Delivery.Broken() || snapshot.Counts.Undelivered != 0 {
		t.Fatalf("disabled channel raised a delivery alarm: %#v", snapshot.Delivery)
	}
}

// watchAttention de-duplicates streamed updates on the marshalled bytes, so two
// identical states must still produce identical JSON.
func TestAttentionSnapshotStaysByteStable(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {
			Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 3,
			LastError: "boom", NextRetryAt: time.Unix(5000, 0).UTC(),
		},
	}, StateBlocked, "t1")
	first, err := json.Marshal(buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels()))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := json.Marshal(buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("snapshot bytes differ between builds:\n%s\n%s", first, next)
		}
	}
}

// A healthy snapshot stays quiet: the aggregate is present but empty, and no
// per-item delivery or error detail appears. Everything added is additive, so a
// consumer reading only documented v1 fields is unaffected.
func TestHealthyAttentionSnapshotStaysQuiet(t *testing.T) {
	state := deliveryTestState(nil, StateBlocked, "t1")
	encoded, err := json.Marshal(buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"delivery":{}`)) {
		t.Errorf("healthy aggregate is not empty: %s", encoded)
	}
	for _, field := range []string{`"last_error"`, `"next_retry_at"`, `"failed"`, `"retrying"`} {
		if bytes.Contains(encoded, []byte(field)) {
			t.Errorf("healthy snapshot contains %s: %s", field, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte(`"version":2`)) {
		t.Errorf("snapshot version not bumped: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"undelivered":0`)) {
		t.Errorf("undelivered count missing: %s", encoded)
	}
}

func TestAttentionWaybarMarksDeliveryFailure(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {
			Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 8,
			LastError: "no notification server", Abandoned: true,
		},
	}, StateBlocked, "t1")
	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	output := attentionWaybar(snapshot, nil)

	if output.Class != "notify-failed" {
		t.Errorf("class = %q, want notify-failed", output.Class)
	}
	// The suffix is what a user with un-updated CSS actually sees.
	if output.Text != "1!" {
		t.Errorf("text = %q, want 1!", output.Text)
	}
	if !strings.Contains(output.Tooltip, "cannot deliver kitty notifications") {
		t.Errorf("tooltip = %q", output.Tooltip)
	}
	if !strings.Contains(output.Tooltip, "no notification server") {
		t.Errorf("tooltip omits the cause: %q", output.Tooltip)
	}
}

// Pause is intentional; a broken channel is not, so it must win.
func TestAttentionWaybarDeliveryFailureOutranksPause(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {Key: "kitty:blocked:t1", Channel: "kitty", LastError: "boom"},
	}, StateBlocked, "t1")
	state.AttentionPaused = true
	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	output := attentionWaybar(snapshot, nil)

	if output.Class != "notify-failed" {
		t.Errorf("class = %q, want notify-failed to outrank paused", output.Class)
	}
	if !strings.Contains(output.Tooltip, "paused") {
		t.Errorf("tooltip lost the pause state: %q", output.Tooltip)
	}
}

func TestAttentionWaybarStaysCleanWhenHealthy(t *testing.T) {
	state := deliveryTestState(nil, StateBlocked, "t1")
	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	output := attentionWaybar(snapshot, nil)

	if output.Text != "1" || output.Class != string(StateBlocked) {
		t.Errorf("healthy waybar = %+v", output)
	}
}

func TestAttentionStatusHumanShowsUndelivered(t *testing.T) {
	state := deliveryTestState(map[string]NotificationRecord{
		"kitty:blocked:t1": {Key: "kitty:blocked:t1", Channel: "kitty", LastError: "boom"},
	}, StateBlocked, "t1")
	snapshot := buildAttentionSnapshot(state, []AgentState{StateBlocked}, allChannels())

	var out bytes.Buffer
	if err := writeAttentionOutput(&out, attentionOutputHuman, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "undelivered=1") {
		t.Errorf("status = %q", out.String())
	}
}

func TestNotificationDeliveryCheckClassifiesRecords(t *testing.T) {
	now := time.Unix(10000, 0).UTC()
	workspace := func(records map[string]NotificationRecord) []*Workspace {
		return []*Workspace{{
			ID: "ws", Name: "reviewer",
			Panes: map[string]*Pane{"pane": {ID: "pane", Notifications: records}},
		}}
	}

	t.Run("all delivered", func(t *testing.T) {
		check := notificationDeliveryCheck(workspace(map[string]NotificationRecord{
			"kitty:blocked:t1": {Key: "kitty:blocked:t1", Channel: "kitty", SentAt: now},
		}), nil, now)
		if !check.OK || !strings.Contains(check.Detail, "1 delivered") {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("abandoned fails", func(t *testing.T) {
		check := notificationDeliveryCheck(workspace(map[string]NotificationRecord{
			"kitty:blocked:t1": {
				Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 8,
				LastError: "no notification server", Abandoned: true,
			},
		}), nil, now)
		if check.OK {
			t.Fatal("abandoned record did not fail the check")
		}
		for _, want := range []string{"kitty", "reviewer", "abandoned", "no notification server"} {
			if !strings.Contains(check.Detail, want) {
				t.Errorf("detail %q omits %q", check.Detail, want)
			}
		}
	})

	// The clause that would have caught the shutdown phantom.
	t.Run("stale pending fails", func(t *testing.T) {
		check := notificationDeliveryCheck(workspace(map[string]NotificationRecord{
			"kitty:blocked:t1": {
				Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 1,
				LastTriedAt: now.Add(-3 * time.Hour),
			},
		}), nil, now)
		if check.OK {
			t.Fatal("stale reservation did not fail the check")
		}
		if !strings.Contains(check.Detail, "reserved but never attempted") {
			t.Errorf("detail = %q", check.Detail)
		}
	})

	// A delivery still in flight is not a defect.
	t.Run("fresh pending passes", func(t *testing.T) {
		check := notificationDeliveryCheck(workspace(map[string]NotificationRecord{
			"kitty:blocked:t1": {Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 1, LastTriedAt: now},
		}), nil, now)
		if !check.OK {
			t.Fatalf("in-flight delivery failed the check: %+v", check)
		}
	})

	t.Run("list failure reported", func(t *testing.T) {
		check := notificationDeliveryCheck(nil, errors.New("daemon unavailable"), now)
		if check.OK || !strings.Contains(check.Detail, "daemon unavailable") {
			t.Fatalf("check = %+v", check)
		}
	})
}

func TestDesktopNotificationCheckUsesProbe(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		check := desktopNotificationCheck(context.Background(), false, func(context.Context) (string, error) {
			t.Fatal("probed a disabled channel")
			return "", nil
		})
		if !check.OK || check.Detail != "disabled in zka configuration" {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("delivered", func(t *testing.T) {
		check := desktopNotificationCheck(context.Background(), true, func(context.Context) (string, error) {
			return "SwayNotificationCenter 0.12.6", nil
		})
		if !check.OK || !strings.Contains(check.Detail, "SwayNotificationCenter") {
			t.Fatalf("check = %+v", check)
		}
	})

	// A headless origin has no desktop to notify and is not broken.
	t.Run("no session bus", func(t *testing.T) {
		check := desktopNotificationCheck(context.Background(), true, func(context.Context) (string, error) {
			return "", errNoSessionBus
		})
		if !check.OK || check.Detail != "no session bus to probe" {
			t.Fatalf("check = %+v", check)
		}
	})

	t.Run("probe failure", func(t *testing.T) {
		check := desktopNotificationCheck(context.Background(), true, func(context.Context) (string, error) {
			return "", errors.New("ServiceUnknown")
		})
		if check.OK || !strings.Contains(check.Detail, "ServiceUnknown") {
			t.Fatalf("check = %+v", check)
		}
	})
}
