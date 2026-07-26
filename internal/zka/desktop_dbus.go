package zka

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	notificationService    = "org.freedesktop.Notifications"
	notificationObjectPath = dbus.ObjectPath("/org/freedesktop/Notifications")
	notificationInterface  = "org.freedesktop.Notifications"

	// notifierDialBackoff keeps a host without a session bus from redialling on
	// every pane transition. The desktop channel is already confined to a
	// machine with a live local Kitty attachment, so this is insurance.
	notifierDialBackoff = 30 * time.Second
	notifierCallTimeout = 5 * time.Second
)

var (
	errNoSessionBus   = errors.New("no session bus: DBUS_SESSION_BUS_ADDRESS is unset")
	errNotifierClosed = errors.New("desktop notifier is shut down")
)

// dbusNotifier speaks org.freedesktop.Notifications directly. It holds one
// private connection, one signal goroutine, and the id registry that routes a
// button press back to the pane that raised it.
//
// The connection is private rather than dbus.SessionBus() because a shared
// connection would deliver every other user's signals into our channel, and
// closing it would break them.
type dbusNotifier struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	logger  *log.Logger
	handler func(workspaceID, paneID string)

	conn     *dbus.Conn
	object   dbus.BusObject
	signals  chan *dbus.Signal
	registry *notificationRegistry
	closed   bool

	capsKnown bool
	markup    bool
	actions   bool
	server    string

	dialErr    error
	dialAt     time.Time
	dialLogged bool
}

func newDBusNotifier(logger *log.Logger, handler func(workspaceID, paneID string)) *dbusNotifier {
	return &dbusNotifier{
		logger:   logger,
		handler:  handler,
		registry: newNotificationRegistry(),
	}
}

func (n *dbusNotifier) Notify(ctx context.Context, note DesktopNotification) error {
	object, err := n.prepare(ctx)
	if err != nil {
		return err
	}
	n.mu.Lock()
	replaces := n.registry.replaces(note.pane())
	markup, actions := n.markup, n.actions
	n.mu.Unlock()

	var id uint32
	if err := object.CallWithContext(ctx, notificationInterface+".Notify", 0,
		notifyArgs(note, replaces, markup, actions)...).Store(&id); err != nil {
		n.noteCallError(err)
		return err
	}
	n.mu.Lock()
	n.registry.commit(note.pane(), id, time.Now())
	n.mu.Unlock()
	return nil
}

func (n *dbusNotifier) Withdraw(ctx context.Context, workspaceID, paneID string) {
	pane := paneRef{Workspace: workspaceID, Pane: paneID}
	// Never dial merely to withdraw: with no connection there is by definition
	// nothing we posted still on screen.
	n.mu.Lock()
	object := n.object
	if object == nil || n.closed {
		n.mu.Unlock()
		return
	}
	id, ok := n.registry.take(pane, time.Now())
	n.mu.Unlock()
	if !ok {
		return
	}
	// An unknown id is not an error per the specification, so a lost race with
	// the server closing it on its own is silent by design.
	if err := object.CallWithContext(ctx, notificationInterface+".CloseNotification", 0, id).
		Store(); err != nil {
		n.noteCallError(err)
		n.logger.Printf("withdraw desktop notification workspace=%s pane=%s id=%d: %v",
			workspaceID, paneID, id, err)
	}
}

// Probe posts and immediately withdraws a real notification, proving the whole
// path rather than merely that a binary exists on PATH. It returns the server
// name and version so `zka doctor` can name what answered.
func (n *dbusNotifier) Probe(ctx context.Context) (string, error) {
	object, err := n.prepare(ctx)
	if err != nil {
		return "", err
	}
	var id uint32
	note := DesktopNotification{
		Summary: "zka doctor",
		Body:    "desktop notification probe",
		Urgency: urgencyLow,
		Icon:    "dialog-information",
	}
	if err := object.CallWithContext(ctx, notificationInterface+".Notify", 0,
		notifyArgs(note, 0, false, false)...).Store(&id); err != nil {
		n.noteCallError(err)
		return "", err
	}
	if err := object.CallWithContext(ctx, notificationInterface+".CloseNotification", 0, id).
		Store(); err != nil {
		n.noteCallError(err)
		return "", fmt.Errorf("withdraw probe notification: %w", err)
	}
	n.mu.Lock()
	server := n.server
	n.mu.Unlock()
	if server == "" {
		server = "session bus"
	}
	return server, nil
}

func (n *dbusNotifier) Shutdown() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	conn := n.conn
	n.conn, n.object, n.signals = nil, nil, nil
	n.mu.Unlock()
	// Close, never RemoveSignal: godbus closes every registered signal channel
	// from Close, but RemoveSignal only unregisters it. Removing first would
	// leave readSignals blocked on a channel nobody will ever close, and
	// wg.Wait below would deadlock.
	if conn != nil {
		_ = conn.Close()
	}
	n.wg.Wait()
}

// prepare returns a usable bus object, dialling if necessary and learning the
// server's capabilities. Dialling is lazy because NewDaemon runs in every test
// and in the Nix check sandbox, neither of which has a session bus.
func (n *dbusNotifier) prepare(ctx context.Context) (dbus.BusObject, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errNotifierClosed
	}
	if n.object == nil {
		now := time.Now()
		if n.dialErr != nil && now.Before(n.dialAt.Add(notifierDialBackoff)) {
			return nil, n.dialErr
		}
		if err := n.dialLocked(); err != nil {
			n.dialErr, n.dialAt = err, now
			// Log only on the healthy-to-broken edge so a host with no session
			// bus cannot fill the journal one line per pane transition.
			if !n.dialLogged {
				n.dialLogged = true
				n.logger.Printf("desktop notifier unavailable: %v", err)
			}
			return nil, err
		}
		n.dialErr, n.dialLogged = nil, false
	}
	if !n.capsKnown {
		n.refreshCapabilitiesLocked(ctx)
	}
	return n.object, nil
}

func (n *dbusNotifier) dialLocked() error {
	// Guard before touching godbus: with no address it would autolaunch
	// dbus-launch, which a daemon must never do and which would also break the
	// hermeticity of the Nix check sandbox.
	if strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) == "" {
		return errNoSessionBus
	}
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return err
	}
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
		if err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath(notificationObjectPath),
			dbus.WithMatchInterface(notificationInterface),
			dbus.WithMatchMember(member),
		); err != nil {
			_ = conn.Close()
			return err
		}
	}
	signals := make(chan *dbus.Signal, 64)
	conn.Signal(signals)
	n.conn = conn
	n.object = conn.Object(notificationService, notificationObjectPath)
	n.signals = signals
	// A reconnected server issues fresh ids from zero, so carrying the old
	// mappings forward would route a stranger's click into one of our panes.
	n.registry.reset()
	n.capsKnown = false
	n.wg.Add(1)
	go n.readSignals(signals)
	return nil
}

// refreshCapabilitiesLocked asks the server what it supports. A failure here is
// not a dial failure: the bus is fine and the notification server may simply not
// have started yet, so the capabilities stay unknown and are retried on the next
// send rather than poisoning the connection.
func (n *dbusNotifier) refreshCapabilitiesLocked(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, notifierCallTimeout)
	defer cancel()
	var capabilities []string
	if err := n.object.CallWithContext(callCtx, notificationInterface+".GetCapabilities", 0).
		Store(&capabilities); err != nil {
		return
	}
	n.markup, n.actions = false, false
	for _, capability := range capabilities {
		switch capability {
		case "body-markup":
			n.markup = true
		case "actions":
			n.actions = true
		}
	}
	n.capsKnown = true
	n.server = n.serverIdentityLocked(ctx)
}

func (n *dbusNotifier) serverIdentityLocked(ctx context.Context) string {
	callCtx, cancel := context.WithTimeout(ctx, notifierCallTimeout)
	defer cancel()
	var name, vendor, version, spec string
	if err := n.object.CallWithContext(callCtx, notificationInterface+".GetServerInformation", 0).
		Store(&name, &vendor, &version, &spec); err != nil {
		return ""
	}
	return strings.TrimSpace(name + " " + version)
}

func (n *dbusNotifier) readSignals(signals chan *dbus.Signal) {
	defer n.wg.Done()
	for signal := range signals {
		if len(signal.Body) == 0 {
			continue
		}
		// Comma-ok, never a bare assertion: a malformed signal from any
		// application on the bus would otherwise panic the whole daemon.
		id, ok := signal.Body[0].(uint32)
		if !ok {
			continue
		}
		now := time.Now()
		switch signal.Name {
		case notificationInterface + ".ActionInvoked":
			n.mu.Lock()
			pane, known := n.registry.lookup(id, now)
			handler := n.handler
			n.mu.Unlock()
			// Unknown ids belong to other applications: the match rule receives
			// every notification's signals, not only ours.
			if known && handler != nil {
				handler(pane.Workspace, pane.Pane)
			}
		case notificationInterface + ".NotificationClosed":
			n.mu.Lock()
			n.registry.forget(id, now)
			n.mu.Unlock()
		}
	}
	n.dropConnection(signals)
}

// dropConnection retires a connection whose signal channel has closed, which is
// how a disconnect is observed. The next send redials.
func (n *dbusNotifier) dropConnection(signals chan *dbus.Signal) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.signals != signals {
		return
	}
	n.conn, n.object, n.signals = nil, nil, nil
	n.capsKnown = false
	n.registry.reset()
}

func (n *dbusNotifier) noteCallError(err error) {
	if !errors.Is(err, dbus.ErrClosed) {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.conn, n.object, n.signals = nil, nil, nil
	n.capsKnown = false
	n.registry.reset()
}

// notifyArgs is the exact org.freedesktop.Notifications.Notify argument list.
// The concrete Go types are the contract: godbus infers the D-Bus signature from
// them, so an int where an int32 or a byte is required is a runtime InvalidArgs
// rather than a compile error.
func notifyArgs(note DesktopNotification, replaces uint32, markup, actions bool) []any {
	summary, body := note.Summary, note.Body
	if markup {
		summary, body = escapePangoMarkup(summary), escapePangoMarkup(body)
	}
	// Omit the actions entirely when the server does not advertise them, so it
	// never renders a button that cannot do anything.
	buttons := []string{}
	if actions && note.ActionLabel != "" {
		buttons = desktopActions(note.ActionLabel)
	}
	return []any{
		desktopAppName, // app_name       STRING
		replaces,       // replaces_id    UINT32
		note.Icon,      // app_icon       STRING
		summary,        // summary        STRING
		body,           // body           STRING
		buttons,        // actions        ARRAY<STRING>
		map[string]dbus.Variant{ // hints          DICT<STRING,VARIANT>
			"urgency": dbus.MakeVariant(note.Urgency),
		},
		desktopExpireTimeout(note.Urgency), // expire_timeout INT32
	}
}
