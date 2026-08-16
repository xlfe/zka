package zka

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// windowNoticeWorkspace is one authoritative workspace holding a granted
// window. The provider maximum is deliberately generous so the grant is the
// claim's own request unless a test asks otherwise.
func windowNoticeWorkspace(generation uint64, windowSeconds int64, anchor time.Time) *Workspace {
	return &Workspace{
		ID: "0123456789abcdef0123456789abcdef", Name: "reviewer",
		CredentialClaim: &CredentialClaim{
			ProviderSource: "local", Bundle: "work", State: "ready", Generation: generation,
			WindowSeconds: windowSeconds, UpdatedAt: anchor,
			PIVB: &CredentialPIVBManifest{MaxGrantWindowS: 3600},
		},
	}
}

func TestCredentialWindowNoticeSweepAnnouncesEachGrantOnce(t *testing.T) {
	anchor := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	workspace := windowNoticeWorkspace(4, 1800, anchor)
	deadline := anchor.Add(1800 * time.Second)
	states := map[string]*credentialWindowNoticeState{}

	if notices := credentialWindowNoticeSweep([]*Workspace{workspace}, states, anchor.Add(time.Minute)); len(notices) != 0 {
		t.Fatalf("mid-window sweep = %#v, want silence", notices)
	}
	closing := credentialWindowNoticeSweep([]*Workspace{workspace}, states, deadline.Add(-credentialWindowNoticeLead))
	if len(closing) != 1 || closing[0].Kind != credentialWindowNoticeClosing || closing[0].WorkspaceID != workspace.ID {
		t.Fatalf("closing sweep = %#v", closing)
	}
	if body := closing[0].body(); body != "reviewer · work — expires in 2m0s; the next mint will need a YubiKey touch" {
		t.Fatalf("closing body = %q", body)
	}
	// The tick runs once a second; the operator hears about a closing window
	// once, not a hundred and twenty times.
	if repeat := credentialWindowNoticeSweep([]*Workspace{workspace}, states, deadline.Add(-90*time.Second)); len(repeat) != 0 {
		t.Fatalf("repeated closing sweep = %#v", repeat)
	}

	expired := credentialWindowNoticeSweep([]*Workspace{workspace}, states, deadline)
	if len(expired) != 1 || expired[0].Kind != credentialWindowNoticeExpired {
		t.Fatalf("expiry sweep = %#v", expired)
	}
	if body := expired[0].body(); body != "reviewer · work — expired; the next mint needs a YubiKey touch" {
		t.Fatalf("expired body = %q", body)
	}
	if repeat := credentialWindowNoticeSweep([]*Workspace{workspace}, states, deadline.Add(time.Hour)); len(repeat) != 0 {
		t.Fatalf("repeated expiry sweep = %#v", repeat)
	}

	// A windowed re-claim always bumps the generation, so the new grant gets its
	// own pair of notices rather than inheriting the old grant's silence.
	regranted := deadline.Add(time.Hour)
	workspace.CredentialClaim.Generation, workspace.CredentialClaim.UpdatedAt = 5, regranted
	renewed := credentialWindowNoticeSweep([]*Workspace{workspace}, states, regranted.Add(1800*time.Second-credentialWindowNoticeLead))
	if len(renewed) != 1 || renewed[0].Kind != credentialWindowNoticeClosing {
		t.Fatalf("re-granted closing sweep = %#v", renewed)
	}
	if state := states[workspace.ID]; state == nil || state.Generation != 5 || !state.PreSent || state.ExpirySent {
		t.Fatalf("re-granted state = %#v", states[workspace.ID])
	}
}

func TestCredentialWindowNoticeSweepSkipsWarningsItCannotUse(t *testing.T) {
	anchor := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	states := map[string]*credentialWindowNoticeState{}

	// A grant barely longer than the lead would warn almost as soon as it was
	// asked for, so short windows get their expiry and nothing else.
	short := windowNoticeWorkspace(4, 180, anchor)
	shortDeadline := anchor.Add(180 * time.Second)
	if notices := credentialWindowNoticeSweep([]*Workspace{short}, states, shortDeadline.Add(-credentialWindowNoticeLead)); len(notices) != 0 {
		t.Fatalf("short-window closing sweep = %#v, want no warning", notices)
	}
	if notices := credentialWindowNoticeSweep([]*Workspace{short}, states, shortDeadline); len(notices) != 1 ||
		notices[0].Kind != credentialWindowNoticeExpired {
		t.Fatalf("short-window expiry sweep = %#v", notices)
	}

	// A daemon that starts after a window closed announces nothing: the operator
	// cannot act on a grant that ended while zkad was down.
	restarted := map[string]*credentialWindowNoticeState{}
	stale := windowNoticeWorkspace(4, 1800, anchor)
	if notices := credentialWindowNoticeSweep([]*Workspace{stale}, restarted, anchor.Add(2*time.Hour)); len(notices) != 0 {
		t.Fatalf("first sweep of a closed window = %#v, want silence", notices)
	}
	if state := restarted[stale.ID]; state == nil || !state.ExpirySent {
		t.Fatalf("closed window state = %#v, want the expiry recorded as said", restarted[stale.ID])
	}

	// Starting up inside the lead is different: that window is still open, and
	// the operator still wants to know it is about to close.
	inside := map[string]*credentialWindowNoticeState{}
	live := windowNoticeWorkspace(4, 1800, anchor)
	notices := credentialWindowNoticeSweep([]*Workspace{live}, inside, anchor.Add(1800*time.Second).Add(-time.Minute))
	if len(notices) != 1 || notices[0].Kind != credentialWindowNoticeClosing || notices[0].Remaining != time.Minute {
		t.Fatalf("first sweep inside the lead = %#v", notices)
	}
}

func TestCredentialWindowNoticeSweepForgetsWhatItNoLongerDescribes(t *testing.T) {
	anchor := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	now := anchor.Add(time.Minute)
	tests := []struct {
		name  string
		amend func(*Workspace)
	}{
		{name: "claim released", amend: func(w *Workspace) { w.CredentialClaim = nil }},
		{name: "claim windowless", amend: func(w *Workspace) { w.CredentialClaim.WindowSeconds = 0 }},
		{name: "claim not ready", amend: func(w *Workspace) { w.CredentialClaim.State = "pending" }},
		{name: "workspace mirrored", amend: func(w *Workspace) { w.RemoteHost = "devbox" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := windowNoticeWorkspace(4, 1800, anchor)
			states := map[string]*credentialWindowNoticeState{}
			credentialWindowNoticeSweep([]*Workspace{workspace}, states, now)
			if states[workspace.ID] == nil {
				t.Fatal("a live windowed claim left no state to forget")
			}
			test.amend(workspace)
			if notices := credentialWindowNoticeSweep([]*Workspace{workspace}, states, now); len(notices) != 0 {
				t.Fatalf("sweep = %#v, want silence", notices)
			}
			if len(states) != 0 {
				t.Fatalf("states = %#v, want the entry dropped with the grant", states)
			}
		})
	}
}

func TestCredentialWindowNoticeDeliveryHonoursNotificationPolicy(t *testing.T) {
	notice := credentialWindowNotice{
		WorkspaceID: "0123456789abcdef0123456789abcdef", WorkspaceName: "reviewer",
		Bundle: "work", Kind: credentialWindowNoticeClosing, Remaining: 2 * time.Minute,
	}
	tests := []struct {
		name      string
		desktop   bool
		ntfy      bool
		wantNotes int
		wantCalls int
	}{
		// Unlike a private-key use notice, this one is informational: an operator
		// who turned a channel off does not get it back through this path.
		{name: "no channel enabled"},
		{name: "desktop only", desktop: true, wantNotes: 1},
		{name: "ntfy only", ntfy: true, wantCalls: 1},
		{name: "both channels", desktop: true, ntfy: true, wantNotes: 1, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notifier, runner := &fakeNotifier{}, quietRunner()
			d := &Daemon{desktop: notifier, runner: runner, logger: log.New(io.Discard, "", 0)}
			d.config.Notifications.DesktopEnabled, d.config.Notifications.NtfyEnabled = test.desktop, test.ntfy
			d.config.Notifications.NtfyCommand = "ntfy-send"
			d.notifyCredentialWindow(context.Background(), notice)

			notes := notifier.Notes()
			if len(notes) != test.wantNotes {
				t.Fatalf("desktop notes = %#v, want %d", notes, test.wantNotes)
			}
			if len(notes) == 1 {
				note := notes[0]
				if note.WorkspaceID != notice.WorkspaceID || note.PaneID != credentialWindowNoticePane ||
					note.Summary != "PIVB grant window" || note.Icon != "dialog-password" || note.Urgency != urgencyNormal {
					t.Fatalf("desktop note = %#v", note)
				}
				if !strings.Contains(note.Body, "reviewer · work — expires in 2m0s") {
					t.Fatalf("desktop body = %q", note.Body)
				}
			}
			calls := runner.Calls()
			if len(calls) != test.wantCalls {
				t.Fatalf("ntfy calls = %#v, want %d", calls, test.wantCalls)
			}
			if len(calls) == 1 {
				call := calls[0]
				if call.Name != "ntfy-send" || len(call.Args) != 7 || call.Args[1] != "PIVB grant window" ||
					call.Args[3] != "3" || call.Args[5] != "hourglass" || !strings.Contains(call.Args[6], "expires in 2m0s") {
					t.Fatalf("ntfy call = %#v", call)
				}
			}
		})
	}
}

// The tick is where the schedule and the delivery meet, so drive one grant
// through the daemon method the credential route loop calls.
func TestCredentialWindowNoticesReachTheOperatorFromTheTick(t *testing.T) {
	d, _, workspace, owner := windowedCredentialDaemon(t, 3600)
	d.config.Notifications.DesktopEnabled = true
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", owner.ID, 900); err != nil {
		t.Fatal(err)
	}
	notifier := fakeDesktop(t, d)
	deadline := credentialClaimSnapshot(t, d, workspace.ID).UpdatedAt.Add(900 * time.Second)

	d.checkCredentialWindowNotices(deadline.Add(-10 * time.Minute))
	if notes := notifier.Notes(); len(notes) != 0 {
		t.Fatalf("mid-window tick notified %#v", notes)
	}
	d.checkCredentialWindowNotices(deadline.Add(-credentialWindowNoticeLead))
	waitFor(t, func() bool { return len(notifier.Notes()) == 1 })
	if note := notifier.Notes()[0]; note.WorkspaceID != workspace.ID ||
		!strings.Contains(note.Body, "reviewer · work — expires in 2m0s") {
		t.Fatalf("closing note = %#v", notifier.Notes()[0])
	}
	d.checkCredentialWindowNotices(deadline)
	waitFor(t, func() bool { return len(notifier.Notes()) == 2 })
	if note := notifier.Notes()[1]; !strings.Contains(note.Body, "expired; the next mint needs a YubiKey touch") {
		t.Fatalf("expiry note = %#v", note)
	}
}
