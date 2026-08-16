package zka

import (
	"context"
	"strings"
	"time"
)

const (
	// credentialWindowNoticeLead is how much warning an operator gets before a
	// grant window closes. It is short on purpose: the notice exists so a touch
	// arriving mid-task is expected, not so the window can be renewed in time.
	credentialWindowNoticeLead = 2 * time.Minute
	// credentialWindowNoticeMinimum is the shortest grant that earns a warning
	// at all. A window barely longer than the lead would announce its own
	// closure almost as soon as it opened, which tells the operator nothing
	// they did not just ask for.
	credentialWindowNoticeMinimum = 4 * time.Minute

	// credentialWindowNoticePane is the synthetic pane these notices are keyed
	// by. Grant windows belong to the workspace, not to any one pane, and the
	// notification registry keys on workspace and pane together.
	credentialWindowNoticePane = "credential-pivb-window"

	credentialWindowNoticeSummary = "PIVB grant window"
	credentialWindowNoticeClosing = "closing"
	credentialWindowNoticeExpired = "expired"
)

// credentialWindowNoticeState is what the sweep remembers between ticks. It is
// per generation because a windowed re-claim always bumps the generation, so a
// changed generation is a new grant and deserves its own pair of notices.
type credentialWindowNoticeState struct {
	Generation uint64
	PreSent    bool
	ExpirySent bool
}

type credentialWindowNotice struct {
	WorkspaceID   string
	WorkspaceName string
	Bundle        string
	Kind          string
	Remaining     time.Duration
}

func (n credentialWindowNotice) body() string {
	subject := n.WorkspaceName + " · " + n.Bundle
	if n.Kind == credentialWindowNoticeExpired {
		return subject + " — expired; the next mint needs a YubiKey touch"
	}
	return subject + " — expires in " + n.Remaining.String() + "; the next mint will need a YubiKey touch"
}

// credentialWindowNoticeSweep decides which grant-window notices are due. It is
// pure so the schedule is a table test rather than a timing test: the caller
// hands it the workspaces this node is authoritative for, the state left by the
// previous sweep, and the clock. States are mutated in place — entries reset on
// a generation change and disappear with the claim they described.
func credentialWindowNoticeSweep(workspaces []*Workspace, states map[string]*credentialWindowNoticeState, now time.Time) []credentialWindowNotice {
	var notices []credentialWindowNotice
	windowed := make(map[string]struct{}, len(workspaces))
	for _, workspace := range workspaces {
		if workspace == nil || workspace.RemoteHost != "" {
			continue
		}
		claim := workspace.CredentialClaim
		if claim == nil || claim.State != "ready" {
			continue
		}
		_, granted, deadline := credentialClaimWindow(claim)
		if deadline == 0 {
			continue
		}
		windowed[workspace.ID] = struct{}{}
		closesAt := time.Unix(deadline, 0)
		state := states[workspace.ID]
		if state == nil || state.Generation != claim.Generation {
			state = &credentialWindowNoticeState{Generation: claim.Generation}
			states[workspace.ID] = state
			// A grant first seen already closed is history rather than news. A
			// restarted daemon must not announce windows that ended while it was
			// down, and a claim whose provider grants no window at all lands here
			// with a deadline already in the past.
			state.ExpirySent = !closesAt.After(now)
		}
		// A deadline is a whole unix second, so the countdown is measured from the
		// current one. Measuring from a tick that happened to fire mid-second
		// would render "expires in 1m59s" and tell the operator nothing extra.
		remaining := closesAt.Sub(now.Truncate(time.Second))
		switch {
		case remaining <= 0:
			if state.ExpirySent {
				continue
			}
			state.ExpirySent = true
			notices = append(notices, credentialWindowNotice{
				WorkspaceID: workspace.ID, WorkspaceName: credentialWindowWorkspaceName(workspace),
				Bundle: claim.Bundle, Kind: credentialWindowNoticeExpired,
			})
		case remaining <= credentialWindowNoticeLead:
			if state.PreSent || time.Duration(granted)*time.Second < credentialWindowNoticeMinimum {
				continue
			}
			state.PreSent = true
			notices = append(notices, credentialWindowNotice{
				WorkspaceID: workspace.ID, WorkspaceName: credentialWindowWorkspaceName(workspace),
				Bundle: claim.Bundle, Kind: credentialWindowNoticeClosing, Remaining: remaining,
			})
		}
	}
	for workspaceID := range states {
		if _, ok := windowed[workspaceID]; !ok {
			delete(states, workspaceID)
		}
	}
	return notices
}

func credentialWindowWorkspaceName(workspace *Workspace) string {
	if name := strings.TrimSpace(workspace.Name); name != "" {
		return name
	}
	return shortID(workspace.ID)
}

// checkCredentialWindowNotices runs one sweep from the credential route tick.
// Nothing here may block that tick: the state snapshot and the sweep are taken
// under their locks and delivery is handed to workers afterwards.
func (d *Daemon) checkCredentialWindowNotices(now time.Time) {
	d.mu.Lock()
	workspaces := credentialWindowNoticeWorkspaces(d.state)
	d.mu.Unlock()
	d.credentialMu.Lock()
	notices := credentialWindowNoticeSweep(workspaces, d.credentialWindowState, now)
	d.credentialMu.Unlock()
	for _, notice := range notices {
		// Journal it whether or not a channel is enabled: this is the record that
		// the grant closed, and a headless origin has no other one.
		d.logger.Printf("PIVB grant window %s workspace=%s bundle=%s", notice.Kind, notice.WorkspaceID, notice.Bundle)
		d.startWorker(func(ctx context.Context) { d.notifyCredentialWindow(ctx, notice) })
	}
}

// credentialWindowNoticeWorkspaces copies what the sweep reads out from under
// d.mu. Workspace.Clone is a JSON round trip of every pane and attachment,
// which a once-a-second tick cannot afford; the rule reads claim identity, the
// window the claim carries, and the provider maximum that clamped it.
func credentialWindowNoticeWorkspaces(state StateData) []*Workspace {
	var workspaces []*Workspace
	for _, workspace := range state.Workspaces {
		if workspace.RemoteHost != "" || workspace.CredentialClaim == nil {
			continue
		}
		claim := *workspace.CredentialClaim
		if claim.PIVB != nil {
			claim.PIVB = &CredentialPIVBManifest{MaxGrantWindowS: claim.PIVB.MaxGrantWindowS}
		}
		workspaces = append(workspaces, &Workspace{ID: workspace.ID, Name: workspace.Name, CredentialClaim: &claim})
	}
	return workspaces
}

// notifyCredentialWindow delivers one grant-window notice. Unlike a private-key
// use notice, this one is informational — the operator loses nothing but a
// touch by missing it — so it honours both notification channels' settings
// instead of forcing itself through.
func (d *Daemon) notifyCredentialWindow(ctx context.Context, notice credentialWindowNotice) {
	policy := d.config.NotificationPolicy()
	body := notice.body()
	if policy.enabled("kitty") && d.desktop != nil {
		notifyCtx, cancel := context.WithTimeout(ctx, notifierCallTimeout)
		err := d.desktop.Notify(notifyCtx, DesktopNotification{
			WorkspaceID: notice.WorkspaceID, PaneID: credentialWindowNoticePane,
			Summary: credentialWindowNoticeSummary, Body: body, Urgency: urgencyNormal, Icon: "dialog-password",
		})
		cancel()
		if err != nil && ctx.Err() == nil {
			d.logger.Printf("credential window desktop notice failed workspace=%s: %v", notice.WorkspaceID, err)
		}
	}
	if !policy.enabled("ntfy") {
		return
	}
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, err := d.runner.Run(notifyCtx, d.config.Notifications.NtfyCommand,
		"-T", credentialWindowNoticeSummary, "-p", "3", "-g", "hourglass", body)
	if err != nil && ctx.Err() == nil {
		d.logger.Printf("credential window ntfy notice failed workspace=%s: %v", notice.WorkspaceID, err)
	}
}
