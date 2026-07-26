package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// seedDesktopTarget makes a pane eligible for the desktop channel: blocked, with
// a ready local Kitty attachment the user is not already looking at.
//
// These tests then drive sendDesktop/sendNtfy directly rather than going through
// afterTransition, which first calls reconcile and would recapture the injected
// attachment from a fake Kitty that reports no windows. Channel eligibility is
// covered by TestNotificationChannelsCanBeDisabledIndependently and the remote
// mirror test; what is under test here is the delivery policy.
func seedDesktopTarget(t *testing.T, d *Daemon, workspace *Workspace, turn string) (*Workspace, *Pane) {
	t.Helper()
	pane := firstPane(workspace)
	d.mu.Lock()
	actual := d.state.Workspaces[workspace.ID]
	actual.Attachments["local"] = &Attachment{
		ID: "local", Node: d.state.Node, Endpoint: "unix:/kitty", Status: AttachmentReady,
		PID: 4242, Views: readyView(pane.ID, 7),
	}
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	return setPaneForNotification(t, d, workspace, StateBlocked, turn)
}

func paneRecord(t *testing.T, d *Daemon, workspaceID, paneID, channel string) (NotificationRecord, bool) {
	t.Helper()
	got, err := d.getWorkspace(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range got.Panes[paneID].SortedNotifications() {
		if record.Channel == channel {
			return record, true
		}
	}
	return NotificationRecord{}, false
}

// The mirror of TestNtfyFailureIsRetriedAndRecorded, which did not exist for the
// desktop channel. Its absence is why a transport that never worked went
// unnoticed for the life of the project.
func TestDesktopFailureIsRetriedLoggedAndRecorded(t *testing.T) {
	d, journal, err := newTestDaemonWithLog(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.NtfyEnabled = false
	notifier := fakeDesktop(t, d)
	notifier.SetError(errors.New("no notification server"))
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-desktop-fail")

	d.sendDesktop(context.Background(), workspace, pane)

	waitFor(t, func() bool { return len(notifier.Notes()) == notificationSendAttempts })
	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && record.LastError != ""
	})

	record, _ := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
	if !strings.Contains(record.LastError, "no notification server") {
		t.Errorf("last error = %q", record.LastError)
	}
	if record.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", record.Attempts)
	}
	if record.NextRetryAt.IsZero() {
		t.Error("no retry scheduled")
	}
	if notificationRecordStatus(record) != "retrying" {
		t.Errorf("status = %q, want retrying", notificationRecordStatus(record))
	}
	waitFor(t, func() bool {
		return strings.Contains(journal.String(), "desktop delivery failed workspace=")
	})
}

// Both channels must retry identically. The asymmetry between them is the defect
// this guards against reintroducing.
func TestDesktopAndNtfyShareOneRetryPolicy(t *testing.T) {
	for _, channel := range []string{"kitty", "ntfy"} {
		t.Run(channel, func(t *testing.T) {
			runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
				if name == "ntfy-send" {
					return "", "", errors.New("push refused")
				}
				if name == "kitten" && strings.Contains(strings.Join(args, " "), " ls") {
					return "[]", "", nil
				}
				return "", "", nil
			}}
			d, err := newTestDaemon(t, t.TempDir(), runner)
			if err != nil {
				t.Fatal(err)
			}
			d.config.Notifications.DesktopEnabled = channel == "kitty"
			d.config.Notifications.NtfyEnabled = channel == "ntfy"
			notifier := fakeDesktop(t, d)
			notifier.SetError(errors.New("no notification server"))
			workspace := createTestWorkspace(t, d, 1)
			workspace, pane := seedDesktopTarget(t, d, workspace, "turn-"+channel)

			if channel == "kitty" {
				d.sendDesktop(context.Background(), workspace, pane)
			} else {
				d.sendNtfy(context.Background(), workspace, pane)
			}

			waitFor(t, func() bool {
				record, ok := paneRecord(t, d, workspace.ID, pane.ID, channel)
				return ok && record.LastError != ""
			})
			attempts := len(notifier.Notes())
			if channel == "ntfy" {
				attempts = 0
				for _, call := range runner.Calls() {
					if call.Name == "ntfy-send" {
						attempts++
					}
				}
			}
			if attempts != notificationSendAttempts {
				t.Fatalf("%s transport attempts = %d, want %d", channel, attempts, notificationSendAttempts)
			}
		})
	}
}

// A reservation taken while the daemon is closing used to stay in the ledger
// forever with neither SentAt nor LastError: never delivered, never retried.
func TestClosedDaemonRecordsUndeliveredNotification(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-shutdown")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d.sendDesktop(context.Background(), workspace, pane)

	record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
	if !ok {
		t.Fatal("no record for a reservation taken during shutdown")
	}
	if !record.SentAt.IsZero() {
		t.Error("notification reported as sent after shutdown")
	}
	if !strings.Contains(record.LastError, "shutting down") {
		t.Errorf("last error = %q, want it to name the shutdown", record.LastError)
	}
}

// A SIGKILL mid-delivery leaves the same phantom across restarts. At process
// start there is by definition no in-flight delivery, so this is exact.
func TestSweepInFlightNotificationsMakesPhantomsRetryable(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	workspace := &Workspace{
		ID: "ws",
		Panes: map[string]*Pane{
			"p": {ID: "p", Notifications: map[string]NotificationRecord{
				"kitty:blocked:t1": {Key: "kitty:blocked:t1", Channel: "kitty", Attempts: 1},
				"ntfy:blocked:t1":  {Key: "ntfy:blocked:t1", Channel: "ntfy", SentAt: now},
				"kitty:done:t0":    {Key: "kitty:done:t0", Channel: "kitty", LastError: "boom"},
			}},
		},
	}
	if swept := sweepInFlightNotifications(workspace, now); swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	records := workspace.Panes["p"].Notifications
	if got := records["kitty:blocked:t1"]; got.LastError == "" || !got.NextRetryAt.IsZero() {
		t.Errorf("phantom = %#v, want a retryable failure due immediately", got)
	}
	if got := records["ntfy:blocked:t1"]; got.LastError != "" {
		t.Error("sweep disturbed a delivered record")
	}
	if got := records["kitty:done:t0"]; got.LastError != "boom" {
		t.Error("sweep disturbed an already-failed record")
	}
}

func TestNotificationRetryBackoffDoublesToCeiling(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, notificationRetryBase},
		{1, notificationRetryBase},
		{2, 2 * notificationRetryBase},
		{3, 4 * notificationRetryBase},
		{20, notificationRetryCeiling},
	} {
		if got := notificationRetryBackoff(tc.attempts); got != tc.want {
			t.Errorf("backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// A failure that heals must actually redeliver, and say so.
func TestFailedNotificationIsRetriedAndRecovers(t *testing.T) {
	d, journal, err := newTestDaemonWithLog(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.NtfyEnabled = false
	notifier := fakeDesktop(t, d)
	notifier.SetError(errors.New("no notification server"))
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-recover")

	d.sendDesktop(context.Background(), workspace, pane)
	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && record.LastError != ""
	})

	// The server comes back and the retry becomes due. There is no fake clock in
	// this codebase, so the schedule is cleared directly.
	notifier.SetError(nil)
	d.mu.Lock()
	records := d.state.Workspaces[workspace.ID].Panes[pane.ID].Notifications
	for key, record := range records {
		record.NextRetryAt = time.Time{}
		records[key] = record
	}
	d.mu.Unlock()

	d.retryFailedNotifications(context.Background())

	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && !record.SentAt.IsZero()
	})
	waitFor(t, func() bool {
		return strings.Contains(journal.String(), "desktop delivery recovered workspace=")
	})
}

// A permanently broken channel must stop, or it becomes its own kind of noise.
func TestNotificationRetryBudgetIsBounded(t *testing.T) {
	d, journal, err := newTestDaemonWithLog(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.NtfyEnabled = false
	notifier := fakeDesktop(t, d)
	notifier.SetError(errors.New("no notification server"))
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-budget")

	d.sendDesktop(context.Background(), workspace, pane)
	for i := 0; i < notificationRetryBudget+4; i++ {
		waitFor(t, func() bool {
			record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
			return ok && record.LastError != ""
		})
		d.mu.Lock()
		records := d.state.Workspaces[workspace.ID].Panes[pane.ID].Notifications
		for key, record := range records {
			record.NextRetryAt = time.Time{}
			records[key] = record
		}
		d.mu.Unlock()
		d.retryFailedNotifications(context.Background())
	}

	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && record.Abandoned
	})
	record, _ := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
	if record.Attempts > notificationRetryBudget {
		t.Errorf("attempts = %d, want at most %d", record.Attempts, notificationRetryBudget)
	}
	if notificationRecordStatus(record) != "failed" {
		t.Errorf("status = %q, want failed", notificationRecordStatus(record))
	}
	if !strings.Contains(journal.String(), "desktop delivery abandoned workspace=") {
		t.Error("abandoning a channel was not reported")
	}
	// An abandoned record must never be admitted again.
	admission := d.reserveNotification(workspace.ID, pane.ID, record.Key, "kitty")
	if admission.Allowed || admission.Reason != notificationReasonAbandoned {
		t.Errorf("admission = %+v, want refusal for a spent budget", admission)
	}
}

// Retrying a pane the user has already dealt with would notify about stale news.
func TestRetryIgnoresResolvedPanes(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.NtfyEnabled = false
	notifier := fakeDesktop(t, d)
	notifier.SetError(errors.New("no notification server"))
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-resolved")
	d.sendDesktop(context.Background(), workspace, pane)
	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && record.LastError != ""
	})

	// The user attends to the pane; the failed record is now dead news.
	if _, err := d.markSeen(workspace.ID, pane.ID); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	records := d.state.Workspaces[workspace.ID].Panes[pane.ID].Notifications
	for key, record := range records {
		record.NextRetryAt = time.Time{}
		records[key] = record
	}
	d.mu.Unlock()

	if due := d.pendingNotificationRetries(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("pending retries for a seen pane = %#v", due)
	}
}

// A paused queue must not be redelivered behind the user's back.
func TestRetryRespectsAttentionPause(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Notifications.NtfyEnabled = false
	notifier := fakeDesktop(t, d)
	notifier.SetError(errors.New("no notification server"))
	workspace := createTestWorkspace(t, d, 1)
	workspace, pane := seedDesktopTarget(t, d, workspace, "turn-paused")
	d.sendDesktop(context.Background(), workspace, pane)
	waitFor(t, func() bool {
		record, ok := paneRecord(t, d, workspace.ID, pane.ID, "kitty")
		return ok && record.LastError != ""
	})

	d.mu.Lock()
	d.state.AttentionPaused = true
	records := d.state.Workspaces[workspace.ID].Panes[pane.ID].Notifications
	for key, record := range records {
		record.NextRetryAt = time.Time{}
		records[key] = record
	}
	d.mu.Unlock()

	if due := d.pendingNotificationRetries(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("pending retries while paused = %#v", due)
	}
}

// Without pruning, a long-lived pane accumulates one record per turn per channel
// forever, and remote mirroring copies them all forward on every apply.
func TestNotificationRecordsArePruned(t *testing.T) {
	now := time.Unix(9000, 0).UTC()
	pane := &Pane{
		ID: "p", LastTurnID: "current",
		Notifications: map[string]NotificationRecord{
			"kitty:blocked:current": {Key: "kitty:blocked:current", Channel: "kitty"},
			"kitty:blocked:recent":  {Key: "kitty:blocked:recent", Channel: "kitty", LastTriedAt: now.Add(-time.Hour)},
			"kitty:blocked:stale":   {Key: "kitty:blocked:stale", Channel: "kitty", LastTriedAt: now.Add(-48 * time.Hour)},
			"ntfy:blocked:ancient":  {Key: "ntfy:blocked:ancient", Channel: "ntfy"},
		},
	}
	pruneNotificationRecords(pane, now)
	if _, ok := pane.Notifications["kitty:blocked:current"]; !ok {
		t.Error("pruned the current event")
	}
	if _, ok := pane.Notifications["kitty:blocked:recent"]; !ok {
		t.Error("pruned a record inside the TTL")
	}
	if _, ok := pane.Notifications["kitty:blocked:stale"]; ok {
		t.Error("kept a record past the TTL")
	}
	if _, ok := pane.Notifications["ntfy:blocked:ancient"]; ok {
		t.Error("kept an undated record from an earlier event")
	}
}

func TestNotificationRecordStatusClassifies(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	for _, tc := range []struct {
		name   string
		record NotificationRecord
		want   string
	}{
		{"sent", NotificationRecord{SentAt: now}, "sent"},
		{"pending", NotificationRecord{}, "pending"},
		{"retrying", NotificationRecord{LastError: "boom"}, "retrying"},
		{"failed", NotificationRecord{LastError: "boom", Abandoned: true}, "failed"},
	} {
		if got := notificationRecordStatus(tc.record); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSortedNotificationsIsDeterministic(t *testing.T) {
	pane := &Pane{Notifications: map[string]NotificationRecord{
		"ntfy:b:2":  {Key: "ntfy:b:2", Channel: "ntfy"},
		"kitty:a:1": {Key: "kitty:a:1", Channel: "kitty"},
		"kitty:a:2": {Key: "kitty:a:2", Channel: "kitty"},
	}}
	for i := 0; i < 8; i++ {
		got := pane.SortedNotifications()
		if len(got) != 3 || got[0].Key != "kitty:a:1" || got[1].Key != "kitty:a:2" || got[2].Key != "ntfy:b:2" {
			t.Fatalf("order = %#v", got)
		}
	}
}
