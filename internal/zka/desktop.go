package zka

import (
	"context"
	"strings"
	"time"
)

// DesktopNotifier delivers actionable pane notifications to the local desktop
// and routes the user's response back. Delivery is fire-and-forget: Notify
// returns once the notification server has accepted the message, and a button
// press arrives arbitrarily later through the handler supplied at construction.
//
// The previous transport asked Kitty to run `kitten notify` in a child with no
// controlling terminal, which could never work because that kitten writes an
// OSC 99 escape sequence to a tty. Nothing here goes through Kitty.
type DesktopNotifier interface {
	Notify(ctx context.Context, note DesktopNotification) error
	Withdraw(ctx context.Context, workspaceID, paneID string)
	// Probe proves the channel end to end and returns a description of the
	// server for `zka doctor`. It must never block waiting for the user.
	Probe(ctx context.Context) (string, error)
	Shutdown()
}

// noopDesktopNotifier serves a headless origin: there is no session bus to
// dial, and pretending otherwise would only produce backoff noise. Notify
// still errors so a hypothetical delivery attempt is recorded honestly.
type noopDesktopNotifier struct{}

func (noopDesktopNotifier) Notify(context.Context, DesktopNotification) error { return errNoSessionBus }
func (noopDesktopNotifier) Withdraw(context.Context, string, string)          {}
func (noopDesktopNotifier) Probe(context.Context) (string, error)             { return "", errNoSessionBus }
func (noopDesktopNotifier) Shutdown()                                         {}

// DesktopNotification is one pane's notification. It carries pane identity
// rather than Kitty identity: the action fires arbitrarily later, so the
// attachment and view must be re-resolved from live state, never captured.
type DesktopNotification struct {
	WorkspaceID string
	PaneID      string
	Summary     string
	Body        string
	Urgency     byte
	Icon        string
	ActionLabel string
}

func (n DesktopNotification) pane() paneRef {
	return paneRef{Workspace: n.WorkspaceID, Pane: n.PaneID}
}

// paneRef is the notification registry's key. A pane is unique only within its
// workspace, so both halves are required.
type paneRef struct {
	Workspace string
	Pane      string
}

// Urgency levels from the freedesktop notification specification. They are
// transmitted as a byte hint, so the type matters on the wire.
const (
	urgencyLow      byte = 0
	urgencyNormal   byte = 1
	urgencyCritical byte = 2
)

const (
	desktopAppName     = "zka"
	desktopActionLabel = "Focus"

	// notificationRecentWindow is how long a closed notification id still
	// resolves to its pane. Signals are not ordered across members, so an
	// ActionInvoked can be processed after the NotificationClosed for the same
	// id; without this window that click would be silently dropped.
	notificationRecentWindow = 30 * time.Second
)

// desktopNotification maps pane state onto the freedesktop presentation. It is
// pure so the urgency and icon table is a table test rather than a bus
// integration test. The icon names are real freedesktop theme names; the
// previous `question`/`info` values were kitten aliases and would not resolve.
func desktopNotification(workspace *Workspace, pane *Pane) DesktopNotification {
	urgency, icon := urgencyNormal, "dialog-information"
	switch pane.State {
	case StateBlocked:
		urgency, icon = urgencyCritical, "dialog-question"
	case StateError:
		urgency, icon = urgencyCritical, "dialog-error"
	}
	return DesktopNotification{
		WorkspaceID: workspace.ID,
		PaneID:      pane.ID,
		Summary:     notificationTitle(workspace, pane),
		Body:        notificationBody(workspace, pane, true),
		Urgency:     urgency,
		Icon:        icon,
		ActionLabel: desktopActionLabel,
	}
}

// desktopExpireTimeout keeps anything the user must act on until it is
// explicitly withdrawn, so Withdraw is the single exit for blocked and error.
// Anything else follows the server's own default.
func desktopExpireTimeout(urgency byte) int32 {
	if urgency == urgencyCritical {
		return 0
	}
	return -1
}

// desktopActions is the (id, label) pair list. "default" is the spec's
// activate-the-body action and servers do not render it as a button, so
// registering both makes clicking the body equivalent to pressing Focus.
func desktopActions(label string) []string {
	return []string{"default", label, "focus", label}
}

// escapePangoMarkup protects a notification against servers that advertise
// body-markup and therefore parse the body as Pango. Agent evidence routinely
// contains & and <, which such a server would drop or fail to render. It is
// applied only when the server advertises the capability, because escaping
// unconditionally would show a literal &amp; everywhere else.
func escapePangoMarkup(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// notificationRegistry maps the server's notification ids to the panes that own
// them, so a button press can be routed and a resolved pane's notification can
// be withdrawn. It holds no I/O and takes an explicit clock so it is unit
// testable.
type notificationRegistry struct {
	live   map[uint32]paneRef
	byPane map[paneRef]uint32
	recent map[uint32]recentEntry
}

type recentEntry struct {
	pane    paneRef
	expires time.Time
}

func newNotificationRegistry() *notificationRegistry {
	return &notificationRegistry{
		live:   map[uint32]paneRef{},
		byPane: map[paneRef]uint32{},
		recent: map[uint32]recentEntry{},
	}
}

// replaces returns the id to pass as replaces_id so a pane updates its existing
// notification in place rather than stacking a new one. Zero means "new".
func (r *notificationRegistry) replaces(pane paneRef) uint32 {
	return r.byPane[pane]
}

// commit records the id the server assigned. When it supersedes an earlier id
// for the same pane the old id is retired, because a replaced notification does
// not reliably produce a NotificationClosed of its own.
func (r *notificationRegistry) commit(pane paneRef, id uint32, now time.Time) {
	if id == 0 {
		return
	}
	if previous, ok := r.byPane[pane]; ok && previous != id {
		delete(r.live, previous)
		r.remember(previous, pane, now)
	}
	r.live[id] = pane
	r.byPane[pane] = id
}

// lookup resolves a signal's notification id. Ids we never posted belong to
// other applications on the bus and must be ignored rather than dispatched.
func (r *notificationRegistry) lookup(id uint32, now time.Time) (paneRef, bool) {
	if pane, ok := r.live[id]; ok {
		return pane, true
	}
	r.reap(now)
	if entry, ok := r.recent[id]; ok {
		return entry.pane, true
	}
	return paneRef{}, false
}

// forget retires an id the server reports closed. The pane mapping is only
// cleared when it still points at this id, so a NotificationClosed for an
// already-superseded id cannot evict its replacement.
func (r *notificationRegistry) forget(id uint32, now time.Time) {
	pane, ok := r.live[id]
	if !ok {
		return
	}
	delete(r.live, id)
	if current, ok := r.byPane[pane]; ok && current == id {
		delete(r.byPane, pane)
	}
	r.remember(id, pane, now)
}

// take removes and returns a pane's live id so it can be closed. It reports
// false when nothing was ever posted, which is what keeps Withdraw from having
// to dial the bus just to discover there is nothing to do.
func (r *notificationRegistry) take(pane paneRef, now time.Time) (uint32, bool) {
	id, ok := r.byPane[pane]
	if !ok {
		return 0, false
	}
	delete(r.byPane, pane)
	delete(r.live, id)
	r.remember(id, pane, now)
	return id, true
}

// reset drops every mapping. A reconnected bus issues fresh ids, so carrying
// the old ones forward would route a stranger's click into a pane.
func (r *notificationRegistry) reset() {
	r.live = map[uint32]paneRef{}
	r.byPane = map[paneRef]uint32{}
	r.recent = map[uint32]recentEntry{}
}

func (r *notificationRegistry) remember(id uint32, pane paneRef, now time.Time) {
	r.reap(now)
	r.recent[id] = recentEntry{pane: pane, expires: now.Add(notificationRecentWindow)}
}

func (r *notificationRegistry) reap(now time.Time) {
	for id, entry := range r.recent {
		if !now.Before(entry.expires) {
			delete(r.recent, id)
		}
	}
}
