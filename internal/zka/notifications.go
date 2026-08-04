package zka

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	// In-send retry wins a race: a busy socket or a DNS blip.
	notificationSendAttempts = 3
	notificationRetryStep    = 250 * time.Millisecond

	// Record-level retry wins an outage: a notification server restarting, or a
	// network down for minutes. The budget and window together bound a
	// permanently broken channel to 8 rounds over two hours, after which the
	// record is abandoned and never retried again.
	notificationRetryBase     = 30 * time.Second
	notificationRetryCeiling  = 15 * time.Minute
	notificationRetryBudget   = 8
	notificationRetryWindow   = 2 * time.Hour
	notificationRetryInterval = 15 * time.Second
	notificationRetryBatch    = 8

	notificationRecordTTL = 24 * time.Hour
)

// NotificationPolicy is the channel-enablement concern lifted out of
// reserveNotification so the attention projection can also answer "was this
// channel even supposed to deliver?" without holding a *Daemon.
type NotificationPolicy struct {
	Desktop bool
	Ntfy    bool
}

func (c Config) NotificationPolicy() NotificationPolicy {
	return NotificationPolicy{
		Desktop: c.Notifications.DesktopEnabled,
		Ntfy:    c.Notifications.NtfyEnabled,
	}
}

func (p NotificationPolicy) enabled(channel string) bool {
	switch channel {
	case "kitty":
		return p.Desktop
	case "ntfy":
		return p.Ntfy
	default:
		return false
	}
}

func notificationTitle(workspace *Workspace, pane *Pane) string {
	name := notificationWorkspaceName(workspace)
	switch pane.State {
	case StateBlocked:
		return name + " needs input"
	case StateError:
		return name + " failed"
	case StateDone:
		return name + " finished"
	default:
		return name
	}
}

func notificationBody(workspace *Workspace, pane *Pane, includeEvidence bool) string {
	detail := notificationStateSummary(pane)
	if includeEvidence {
		if evidence := strings.Join(strings.Fields(pane.Evidence.Detail), " "); evidence != "" {
			detail = evidence
		}
	}
	context := []string{"Workspace: " + notificationWorkspaceName(workspace)}
	if title := strings.TrimSpace(pane.Title); title != "" {
		context = append(context, "Pane: "+title)
	}
	origin := strings.TrimSpace(workspace.Origin.Name)
	if origin == "" {
		origin = strings.TrimSpace(workspace.RemoteHost)
	}
	if origin != "" {
		context = append(context, "Origin: "+origin)
	}
	return detail + "\n\n" + strings.Join(context, " · ")
}

func notificationWorkspaceName(workspace *Workspace) string {
	if workspace == nil {
		return "Workspace"
	}
	if name := strings.TrimSpace(workspace.Name); name != "" {
		return name
	}
	if workspace.ID != "" {
		return "Workspace " + shortID(workspace.ID)
	}
	return "Workspace"
}

func notificationStateSummary(pane *Pane) string {
	agent := "Agent"
	switch strings.ToLower(strings.TrimSpace(pane.Agent)) {
	case "claude":
		agent = "Claude"
	case "codex":
		agent = "Codex"
	}
	switch pane.State {
	case StateBlocked:
		return agent + " needs your input."
	case StateError:
		return agent + " stopped with an error."
	case StateDone:
		return agent + " finished."
	default:
		return agent + " status: " + string(pane.State) + "."
	}
}

func (d *Daemon) afterTransition(ctx context.Context, before AgentState, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	if d.attentionStateEnabled(pane.State) {
		d.reconcile(ctx)
		if fresh, err := d.getWorkspace(workspace.ID); err == nil {
			workspace = fresh
			pane = fresh.Panes[paneID]
		}
		if pane == nil || !d.attentionStateEnabled(pane.State) {
			return
		}
	}
	d.updateKittyState(ctx, workspace, paneID)
	_, focused := attentionPaneView(workspace, paneID)
	if !d.attentionStateEnabled(pane.State) || (pane.State == StateDone && focused) {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if d.config.Notifications.DesktopEnabled {
		if attachment, _ := d.firstUnfocusedView(workspace, paneID); attachment != nil {
			d.sendDesktop(ctx, workspace, pane)
		}
	}
	important := pane.State == StateBlocked || pane.State == StateError || (pane.State == StateDone && !paneAttached(workspace, paneID))
	if important && d.config.Notifications.NtfyEnabled {
		d.sendNtfy(ctx, workspace, pane)
	}
	_ = before
}

func (d *Daemon) afterRemoteTransition(ctx context.Context, workspace *Workspace, paneID string) {
	d.updateKittyState(ctx, workspace, paneID)
	d.afterRemoteTransitionNotification(ctx, workspace, paneID)
}

func (d *Daemon) afterRemoteTransitionNotification(ctx context.Context, workspace *Workspace, paneID string) {
	pane := workspace.Panes[paneID]
	if pane == nil {
		return
	}
	_, focused := attentionPaneView(workspace, paneID)
	if !d.attentionStateEnabled(pane.State) || (pane.State == StateDone && focused) {
		d.closeDesktopNotifications(ctx, workspace, paneID)
		return
	}
	if d.config.Notifications.DesktopEnabled {
		if attachment, _ := d.firstUnfocusedView(workspace, paneID); attachment != nil {
			d.sendDesktop(ctx, workspace, pane)
		}
	}
}

// resumeAttentionNotifications projects the current actionable set back onto
// delivery channels. Notification records de-duplicate anything delivered
// before focus mode, while items that resolved during focus mode are absent.
func (d *Daemon) resumeAttentionNotifications(ctx context.Context) {
	snapshot := d.attentionSnapshot()
	if snapshot.Paused {
		return
	}
	for _, item := range snapshot.Items {
		d.mu.Lock()
		workspace := d.state.Workspaces[item.WorkspaceID]
		if workspace != nil {
			workspace = workspace.Clone()
		}
		d.mu.Unlock()
		if workspace == nil {
			continue
		}
		pane := workspace.Panes[item.PaneID]
		if pane == nil {
			continue
		}
		if d.config.Notifications.DesktopEnabled {
			if attachment, _ := d.firstUnfocusedView(workspace, pane.ID); attachment != nil {
				d.sendDesktop(ctx, workspace, pane)
			}
		}
		important := pane.State == StateBlocked || pane.State == StateError || (pane.State == StateDone && !paneAttached(workspace, pane.ID))
		if workspace.RemoteHost == "" && important && d.config.Notifications.NtfyEnabled {
			d.sendNtfy(ctx, workspace, pane)
		}
	}
}

func paneAttached(workspace *Workspace, paneID string) bool {
	for _, attachment := range workspace.Attachments {
		if attachment.Status != AttachmentReady {
			continue
		}
		if attachment.Transport.Kind == "ssh" && !clientHeartbeatFresh(attachment.ClientHeartbeats[paneID], time.Now().UTC()) {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready {
			return true
		}
	}
	return false
}

func isLocalUnixAttachment(attachment *Attachment, nodeID string) bool {
	return attachment != nil &&
		attachment.Node.ID == nodeID &&
		strings.HasPrefix(attachment.Endpoint, "unix:")
}

func isReadyLocalKittyAttachment(attachment *Attachment, nodeID string) bool {
	return isLocalUnixAttachment(attachment, nodeID) &&
		attachment.Status == AttachmentReady &&
		!attachment.Revoked
}

func (d *Daemon) localNodeID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.Node.ID
}

func (d *Daemon) firstUnfocusedView(workspace *Workspace, paneID string) (*Attachment, RuntimeView) {
	localNodeID := d.localNodeID()
	for _, attachment := range workspace.SortedAttachments() {
		if !isReadyLocalKittyAttachment(attachment, localNodeID) {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready && !view.Focused {
			return attachment, view
		}
	}
	return nil, RuntimeView{}
}

func (d *Daemon) updateKittyState(ctx context.Context, workspace *Workspace, paneIDs ...string) {
	localNodeID := d.localNodeID()
	var selected map[string]bool
	if len(paneIDs) != 0 {
		selected = make(map[string]bool, len(paneIDs))
		for _, paneID := range paneIDs {
			selected[paneID] = true
		}
	}
	for _, attachment := range workspace.SortedAttachments() {
		if !isReadyLocalKittyAttachment(attachment, localNodeID) {
			continue
		}
		updated := false
		for paneID, view := range attachment.Views {
			if selected != nil && !selected[paneID] {
				continue
			}
			pane := workspace.Panes[paneID]
			if pane == nil || !view.Ready {
				continue
			}
			updated = true
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := d.kitty.SetPaneState(callCtx, attachment.Endpoint, view, workspace, pane)
			cancel()
			if err != nil {
				d.logger.Printf("update kitty state workspace=%s pane=%s: %v", workspace.ID, paneID, err)
			}
		}
		if updated {
			d.applyTabTitles(ctx, attachment.Endpoint, workspace)
		}
	}
}

// desiredTabName is the single formula for a managed tab's Kitty name: the
// name the desired topology owns, decorated with the worst state among its
// panes. An empty name means the tab must stay unnamed so Kitty falls back to
// its active window's title.
//
// Having one formula matters. Previously the reconciler wrote the bare title on
// every pass while this path wrote a decorated one, so the two overwrote each
// other, the tab bar flickered, and the captured value oscillated.
func desiredTabName(workspace *Workspace, tabNode Node) string {
	if tabNode.Title == "" {
		return ""
	}
	highest := StateIdle
	for _, paneNode := range tabNode.Children {
		if pane := workspace.Panes[paneNode.PaneID]; pane != nil {
			if statePriority(pane.State) > statePriority(highest) {
				highest = pane.State
			}
		}
	}
	return strings.TrimSpace(stateMarker(highest) + " " + tabNode.Title)
}

// applyTabTitles is the only caller of SetTabTitle. It writes only where the
// live name already differs, so a settled workspace issues no calls at all.
func (d *Daemon) applyTabTitles(ctx context.Context, endpoint string, workspace *Workspace) {
	if len(workspace.Topology.Roots) == 0 {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tree, err := d.kitty.List(callCtx, endpoint)
	cancel()
	if err != nil {
		return
	}
	liveTabs := map[string]kittyTab{}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] == workspace.ID {
					liveTabs[window.UserVars["zka_pane"]] = tab
				}
			}
		}
	}
	seen := map[int64]bool{}
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			tab, ok := liveTabs[tabNode.Children[0].PaneID]
			if !ok || seen[tab.ID] {
				continue
			}
			seen[tab.ID] = true
			want := desiredTabName(workspace, tabNode)
			if canonicalStrippedValue(tab.Title) == canonicalStrippedValue(want) {
				continue
			}
			if want == "" && tab.namedTitle() == "" {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.SetTabTitle(callCtx, endpoint, tab.ID, want)
			cancel()
		}
	}
}

func statePriority(state AgentState) int {
	switch state {
	case StateError:
		return 5
	case StateBlocked:
		return 4
	case StateDone:
		return 3
	case StateWorking:
		return 2
	case StateUnknown:
		return 1
	default:
		return 0
	}
}

func stripStateMarker(title string) string {
	for _, state := range []AgentState{StateError, StateBlocked, StateDone, StateWorking, StateUnknown} {
		prefix := stateMarker(state) + " "
		if strings.HasPrefix(title, prefix) {
			return strings.TrimPrefix(title, prefix)
		}
	}
	return title
}

// sendDesktop posts one pane notification. The channel name and key prefix
// remain "kitty" for historical reasons only: both are persisted in state.json
// and compared by reserveNotification, so renaming them would make every live
// record miss on upgrade and re-fire one notification per actionable pane.
func (d *Daemon) sendDesktop(ctx context.Context, workspace *Workspace, pane *Pane) {
	key := notificationKey("kitty", pane)
	admission := d.reserveNotification(workspace.ID, pane.ID, key, "kitty")
	if !admission.Allowed {
		return
	}
	note := desktopNotification(workspace, pane)
	started := d.startWorker(func(workerCtx context.Context) {
		err := deliverWithRetry(workerCtx, notificationSendAttempts, func(attemptCtx context.Context) error {
			callCtx, cancel := context.WithTimeout(attemptCtx, notifierCallTimeout)
			defer cancel()
			return d.desktop.Notify(callCtx, note)
		})
		record := d.finishNotification(workspace.ID, pane.ID, key, err)
		if err != nil {
			if workerCtx.Err() == nil {
				d.logNotificationFailure("desktop", workspace.ID, pane.ID, record, err)
			}
			return
		}
		if admission.Retry {
			d.logNotificationRecovery("desktop", workspace.ID, pane.ID, record)
		}
	})
	// startWorker refuses once the daemon is closing. Ignoring that left the
	// reservation in place with neither SentAt nor LastError: a phantom that
	// could never be delivered and could never be retried.
	if !started {
		record := d.finishNotification(workspace.ID, pane.ID, key, errDaemonClosing)
		d.logNotificationFailure("desktop", workspace.ID, pane.ID, record, errDaemonClosing)
	}
	_ = ctx
}

// handleDesktopAction runs the notification button's effect. It is called from
// the notifier's signal goroutine, so it must not block: all work goes to a
// daemon worker. Identity is re-resolved from live state rather than captured,
// because the click can arrive long after the notification was posted and the
// attachment it was posted against may since have been replaced.
func (d *Daemon) handleDesktopAction(workspaceID, paneID string) {
	d.startWorker(func(ctx context.Context) {
		workspace, err := d.getWorkspace(workspaceID)
		if err != nil {
			d.logger.Printf("desktop action workspace=%s pane=%s: %v", workspaceID, paneID, err)
			return
		}
		focusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if attachment, _ := d.firstUnfocusedView(workspace, paneID); attachment != nil {
			if err := d.kitty.FocusPane(focusCtx, attachment.Endpoint, workspaceID, paneID); err != nil {
				d.logger.Printf("focus Kitty pane from notification: %v", err)
			}
			if err := focusSwayWindow(focusCtx, d.runner, d.config.Focus.SwayCommand, attachment.PID); err != nil {
				d.logger.Printf("focus Sway window from notification: %v", err)
			}
		}
		if workspace.RemoteHost != "" {
			_, _ = d.remotes.Call(focusCtx, workspace.RemoteHost, "seen",
				workspacePaneRequest{Workspace: workspaceID, Pane: paneID})
		} else {
			_, _ = d.markSeen(workspaceID, paneID)
		}
	})
}

// closeDesktopNotifications withdraws anything still on screen for these panes.
// It no longer filters by attachment: the notifier's registry is the authority
// on what was actually posted, so a pane whose Kitty attachment has gone can
// still be dismissed instead of leaving a bubble nothing can clear.
func (d *Daemon) closeDesktopNotifications(ctx context.Context, workspace *Workspace, paneRef string) {
	for paneID := range workspace.Panes {
		if paneRef != "" && paneID != paneRef {
			continue
		}
		d.desktop.Withdraw(ctx, workspace.ID, paneID)
	}
}

func (d *Daemon) sendNtfy(ctx context.Context, workspace *Workspace, pane *Pane) {
	key := notificationKey("ntfy", pane)
	admission := d.reserveNotification(workspace.ID, pane.ID, key, "ntfy")
	if !admission.Allowed {
		return
	}
	priority, tag := "3", "white_check_mark"
	if pane.State == StateBlocked {
		priority, tag = "5", "warning"
	}
	if pane.State == StateError {
		priority, tag = "5", "rotating_light"
	}
	title, body := notificationTitle(workspace, pane), notificationBody(workspace, pane, d.config.Notifications.NtfyIncludeEvidence)
	err := deliverWithRetry(ctx, notificationSendAttempts, func(attemptCtx context.Context) error {
		callCtx, cancel := context.WithTimeout(attemptCtx, 10*time.Second)
		defer cancel()
		_, _, runErr := d.runner.Run(callCtx, d.config.Notifications.NtfyCommand,
			"-T", title, "-p", priority, "-g", tag, body)
		return runErr
	})
	record := d.finishNotification(workspace.ID, pane.ID, key, err)
	if err != nil {
		if ctx.Err() == nil {
			d.logNotificationFailure("ntfy", workspace.ID, pane.ID, record, err)
		}
		return
	}
	if admission.Retry {
		d.logNotificationRecovery("ntfy", workspace.ID, pane.ID, record)
	}
}

func eventIdentity(pane *Pane) string {
	if pane.LastTurnID != "" {
		return pane.LastTurnID
	}
	return pane.Evidence.Timestamp.Format(time.RFC3339Nano)
}

// notificationKey is the identity of one deliverable event on one channel. It is
// a function rather than two concatenations because the attention projection now
// has to reconstruct it to answer "was the user actually told about this?".
func notificationKey(channel string, pane *Pane) string {
	return channel + ":" + string(pane.State) + ":" + eventIdentity(pane)
}

// notificationAdmission is the answer to the three questions reserveNotification
// used to collapse into one bool: may this channel deliver at all, has this exact
// event already been delivered, and is a failed delivery now due for a retry.
// Keeping them apart is what lets a caller distinguish "suppressed because
// paused" from "suppressed because the retry budget is spent".
type notificationAdmission struct {
	Record  NotificationRecord
	Allowed bool
	Reason  string
	Retry   bool
}

const (
	notificationReasonPaused          = "attention paused"
	notificationReasonChannelDisabled = "channel disabled"
	notificationReasonDuplicate       = "already delivered"
	notificationReasonInFlight        = "delivery in flight"
	notificationReasonRetryPending    = "retry not due"
	notificationReasonAbandoned       = "retry budget spent"
	notificationReasonUnknownPane     = "pane no longer exists"
	notificationReasonAdmitted        = ""
)

var errDaemonClosing = errors.New("daemon is shutting down before delivery")

func (d *Daemon) reserveNotification(workspaceID, paneID, key, channel string) notificationAdmission {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.AttentionPaused {
		return notificationAdmission{Reason: notificationReasonPaused}
	}
	if !d.config.NotificationPolicy().enabled(channel) {
		return notificationAdmission{Reason: notificationReasonChannelDisabled}
	}
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return notificationAdmission{Reason: notificationReasonUnknownPane}
	}
	pane := workspace.Panes[paneID]
	if pane.Notifications == nil {
		pane.Notifications = map[string]NotificationRecord{}
	}
	now := time.Now().UTC()
	record, exists := pane.Notifications[key]
	retry := false
	if exists {
		switch {
		case !record.SentAt.IsZero():
			return notificationAdmission{Record: record, Reason: notificationReasonDuplicate}
		case record.Abandoned:
			return notificationAdmission{Record: record, Reason: notificationReasonAbandoned}
		case record.LastError == "":
			return notificationAdmission{Record: record, Reason: notificationReasonInFlight}
		case now.Before(record.NextRetryAt):
			return notificationAdmission{Record: record, Reason: notificationReasonRetryPending}
		}
		retry = true
	}
	record.Key, record.Channel = key, channel
	record.Attempts++
	record.LastTriedAt = now
	if record.FirstTriedAt.IsZero() {
		record.FirstTriedAt = now
	}
	// Clearing the error is what makes "reserved with no error" unambiguously
	// mean in flight, which is what the startup sweep keys on.
	record.LastError, record.NextRetryAt = "", time.Time{}
	pane.Notifications[key] = record
	_ = d.store.Save(d.state)
	return notificationAdmission{Record: record, Allowed: true, Reason: notificationReasonAdmitted, Retry: retry}
}

// finishNotification records the outcome and schedules the next retry. It
// returns the updated record so callers can log how close a channel is to being
// abandoned.
func (d *Daemon) finishNotification(workspaceID, paneID, key string, err error) NotificationRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace := d.state.Workspaces[workspaceID]
	if workspace == nil || workspace.Panes[paneID] == nil {
		return NotificationRecord{}
	}
	pane := workspace.Panes[paneID]
	record := pane.Notifications[key]
	now := time.Now().UTC()
	if err != nil {
		record.LastError = err.Error()
		record.NextRetryAt = now.Add(notificationRetryBackoff(record.Attempts))
		record.Abandoned = record.Attempts >= notificationRetryBudget ||
			(!record.FirstTriedAt.IsZero() && now.Sub(record.FirstTriedAt) >= notificationRetryWindow)
	} else {
		record.SentAt = now
		record.LastError = ""
		record.NextRetryAt = time.Time{}
		record.Abandoned = false
	}
	pane.Notifications[key] = record
	pruneNotificationRecords(pane, now)
	_ = d.store.Save(d.state)
	return record
}

// notificationRetryBackoff is the record-level schedule: 30s doubling to a 15
// minute ceiling. It is deliberately far slower than the in-send retry, because
// a failure that survived three back-to-back attempts is an outage, not a race.
func notificationRetryBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	backoff := notificationRetryBase
	for i := 1; i < attempts; i++ {
		backoff *= 2
		if backoff >= notificationRetryCeiling {
			return notificationRetryCeiling
		}
	}
	return backoff
}

// deliverWithRetry is the single in-send retry policy for every notification
// channel. sendNtfy had one and sendDesktop had none; that asymmetry, not the
// backoff numbers, is what let a broken transport survive unnoticed.
func deliverWithRetry(ctx context.Context, attempts int, attempt func(context.Context) error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		lastErr = attempt(ctx)
		if lastErr == nil || ctx.Err() != nil {
			return lastErr
		}
		if i == attempts-1 {
			break
		}
		// A timer rather than time.After so a cancelled context does not leak
		// one per abandoned delivery, and a plain return rather than a break
		// inside a select, which would only break the select.
		timer := time.NewTimer(time.Duration(i+1) * notificationRetryStep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
	}
	return lastErr
}

// logNotificationFailure is the only place a delivery failure becomes a journal
// line. Both channels route through it so they cannot drift apart again.
func (d *Daemon) logNotificationFailure(channel, workspaceID, paneID string, record NotificationRecord, err error) {
	if record.Abandoned {
		d.logger.Printf("%s delivery abandoned workspace=%s pane=%s attempts=%d: %v",
			channel, workspaceID, paneID, record.Attempts, err)
		return
	}
	d.logger.Printf("%s delivery failed workspace=%s pane=%s attempt=%d/%d retry_in=%s: %v",
		channel, workspaceID, paneID, record.Attempts, notificationRetryBudget,
		notificationRetryBackoff(record.Attempts).Round(time.Second), err)
}

// logNotificationRecovery closes the loop a failure opened. Without it the
// journal shows only the descent and every past failure reads as current.
func (d *Daemon) logNotificationRecovery(channel, workspaceID, paneID string, record NotificationRecord) {
	d.logger.Printf("%s delivery recovered workspace=%s pane=%s attempts=%d",
		channel, workspaceID, paneID, record.Attempts)
}

// pruneNotificationRecords bounds pane.Notifications. Records for the pane's
// current event are always kept; anything else expires, so a stale failure
// cannot pin the doctor red forever and a long-lived pane does not accumulate
// one record per turn per channel without limit.
func pruneNotificationRecords(pane *Pane, now time.Time) {
	if len(pane.Notifications) == 0 {
		return
	}
	current := eventIdentity(pane)
	for key, record := range pane.Notifications {
		if strings.HasSuffix(key, ":"+current) {
			continue
		}
		stamp := record.LastTriedAt
		if stamp.IsZero() {
			stamp = record.SentAt
		}
		if stamp.IsZero() || now.Sub(stamp) >= notificationRecordTTL {
			delete(pane.Notifications, key)
		}
	}
}

// formatOptionalTime renders a zero time as a dash so a column stays aligned
// and a missing value is visibly missing rather than year zero.
func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

// notificationRecordStatus is the one classifier every surface shares. A
// "pending" record whose attempt is long past is the phantom signature of a
// shutdown that dropped the worker.
func notificationRecordStatus(record NotificationRecord) string {
	switch {
	case !record.SentAt.IsZero():
		return "sent"
	case record.Abandoned:
		return "failed"
	case record.LastError != "":
		return "retrying"
	default:
		return "pending"
	}
}

// SortedNotifications gives every surface a stable order. Map iteration printed
// retained failures differently on each run, so two inspect outputs of the same
// state could not be diffed.
func (p *Pane) SortedNotifications() []NotificationRecord {
	if len(p.Notifications) == 0 {
		return nil
	}
	records := make([]NotificationRecord, 0, len(p.Notifications))
	for _, record := range p.Notifications {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Channel != records[j].Channel {
			return records[i].Channel < records[j].Channel
		}
		return records[i].Key < records[j].Key
	})
	return records
}

// notificationRetry names one due redelivery.
type notificationRetry struct {
	Workspace *Workspace
	PaneID    string
	Channel   string
}

// notificationRetryLoop re-drives deliveries that failed for a reason that may
// since have healed. Without it a failure was permanent: reserveNotification
// refused the key forever, so even `zka attention resume` could not redeliver.
func (d *Daemon) notificationRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(notificationRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.retryFailedNotifications(ctx)
		}
	}
}

func (d *Daemon) retryFailedNotifications(ctx context.Context) {
	for _, retry := range d.pendingNotificationRetries(time.Now().UTC()) {
		pane := retry.Workspace.Panes[retry.PaneID]
		if pane == nil {
			continue
		}
		// Re-enter the channel entry point rather than replaying a captured
		// target: the attachment that existed when delivery first failed may be
		// gone, and firstUnfocusedView is the only correct selector.
		switch retry.Channel {
		case "kitty":
			if attachment, _ := d.firstUnfocusedView(retry.Workspace, retry.PaneID); attachment != nil {
				d.sendDesktop(ctx, retry.Workspace, pane)
			}
		case "ntfy":
			d.sendNtfy(ctx, retry.Workspace, pane)
		}
	}
}

// pendingNotificationRetries is the scan under d.mu. Relevance is the bound that
// matters more than the budget: a record whose pane has moved on, been seen, or
// left the attention set describes news the user no longer needs, and redelivering
// it would be worse than not retrying at all.
func (d *Daemon) pendingNotificationRetries(now time.Time) []notificationRetry {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.AttentionPaused {
		return nil
	}
	policy := d.config.NotificationPolicy()
	var due []notificationRetry
	clones := map[string]*Workspace{}
	for workspaceID, workspace := range d.state.Workspaces {
		for paneID, pane := range workspace.Panes {
			if !d.attentionStateEnabled(pane.State) {
				continue
			}
			if pane.AttentionSeen != "" && pane.AttentionSeen == attentionEventIdentity(pane) {
				continue
			}
			current := eventIdentity(pane)
			for key, record := range pane.Notifications {
				if !policy.enabled(record.Channel) {
					continue
				}
				if record.Abandoned || record.LastError == "" || !record.SentAt.IsZero() {
					continue
				}
				if now.Before(record.NextRetryAt) {
					continue
				}
				if !strings.HasSuffix(key, ":"+current) {
					continue
				}
				clone := clones[workspaceID]
				if clone == nil {
					clone = workspace.Clone()
					clones[workspaceID] = clone
				}
				due = append(due, notificationRetry{
					Workspace: clone, PaneID: paneID, Channel: record.Channel,
				})
				if len(due) >= notificationRetryBatch {
					return due
				}
			}
		}
	}
	return due
}

// sweepInFlightNotifications converts reservations that no worker can still own
// into retryable failures. At process start there is by definition no in-flight
// delivery, so a record with neither SentAt nor LastError is provably lost. This
// is exact, where any timeout-based detector would have to guess.
func sweepInFlightNotifications(workspace *Workspace, now time.Time) int {
	swept := 0
	for _, pane := range workspace.Panes {
		for key, record := range pane.Notifications {
			if !record.SentAt.IsZero() || record.LastError != "" {
				continue
			}
			record.LastError = "daemon restarted before delivery completed"
			record.LastTriedAt = now
			record.NextRetryAt = time.Time{}
			pane.Notifications[key] = record
			swept++
		}
	}
	return swept
}
