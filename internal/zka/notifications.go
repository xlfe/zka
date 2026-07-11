package zka

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

func notificationTitle(session *Session) string {
	switch session.State {
	case StateBlocked:
		return "zka: " + session.Name + " needs input"
	case StateError:
		return "zka: " + session.Name + " failed"
	case StateDone:
		return "zka: " + session.Name + " finished"
	default:
		return "zka: " + session.Name
	}
}

func notificationBody(session *Session) string {
	detail := session.Evidence.Detail
	if detail == "" {
		detail = session.Evidence.Event
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s\nHost: %s\nSession: %s\nFocus: zka focus %s", detail, host, session.ID, session.ID)
}

func (d *Daemon) afterTransition(ctx context.Context, before AgentState, session *Session) {
	if session.State == StateBlocked || session.State == StateError || session.State == StateDone {
		d.reconcile(ctx)
		fresh, err := d.getSession(session.ID)
		if err == nil {
			session = fresh
		}
		if session.State != StateBlocked && session.State != StateError && session.State != StateDone {
			return
		}
	}
	d.updateKittyState(ctx, session)
	if session.State != StateBlocked && session.State != StateError && session.State != StateDone {
		d.closeDesktopNotifications(ctx, session)
		return
	}
	var attached *View
	for _, view := range session.SortedViews() {
		if view.Attached && !view.Focused {
			v := view
			attached = &v
			break
		}
	}
	if attached != nil {
		d.sendDesktop(ctx, *attached, session)
	}
	important := session.State == StateBlocked || session.State == StateError || (session.State == StateDone && !session.AnyAttached())
	if important {
		d.sendNtfy(ctx, session)
	}
}

func (d *Daemon) updateKittyState(ctx context.Context, session *Session) {
	endpoints := map[string]bool{}
	for _, view := range session.SortedViews() {
		if !view.Attached {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.kitty.SetState(callCtx, view.Endpoint, view.WindowID, session)
		cancel()
		if err != nil {
			d.logger.Printf("update kitty state session=%s window=%d: %v", session.ID, view.WindowID, err)
		}
		endpoints[view.Endpoint] = true
	}
	for endpoint := range endpoints {
		d.updateKittyTabTitles(ctx, endpoint)
	}
}

func (d *Daemon) updateKittyTabTitles(ctx context.Context, endpoint string) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	tree, err := d.kitty.List(callCtx, endpoint)
	cancel()
	if err != nil {
		return
	}
	d.mu.Lock()
	states := make(map[string]AgentState, len(d.state.Sessions))
	for id, session := range d.state.Sessions {
		states[id] = session.State
	}
	d.mu.Unlock()
	for _, osw := range tree {
		for _, tab := range osw.Tabs {
			highest := StateIdle
			hasManaged := false
			for _, window := range tab.Windows {
				id := window.UserVars["zka_session"]
				state, ok := states[id]
				if !ok {
					continue
				}
				hasManaged = true
				if statePriority(state) > statePriority(highest) {
					highest = state
				}
			}
			if !hasManaged {
				continue
			}
			base := stripStateMarker(tab.Title)
			title := strings.TrimSpace(stateMarker(highest) + " " + base)
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.SetTabTitle(callCtx, endpoint, tab.ID, title)
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

func (d *Daemon) sendDesktop(ctx context.Context, view View, session *Session) {
	key := "kitty:" + string(session.State) + ":" + eventIdentity(session)
	if !d.reserveNotification(session.ID, key, "kitty") {
		return
	}
	go func() {
		choice, err := d.kitty.Notify(context.WithoutCancel(ctx), view, session)
		if err != nil {
			d.finishNotification(session.ID, key, err)
			return
		}
		d.finishNotification(session.ID, key, nil)
		choice = strings.TrimSpace(choice)
		if choice == "0" || choice == "1" {
			focusCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = d.kitty.Focus(focusCtx, view.Endpoint, session.ID)
			cancel()
		}
	}()
}

func (d *Daemon) closeDesktopNotifications(ctx context.Context, session *Session) {
	for _, view := range session.SortedViews() {
		if view.Endpoint != "" {
			d.kitty.CloseNotification(ctx, view, session.ID)
		}
	}
}

func (d *Daemon) sendNtfy(ctx context.Context, session *Session) {
	key := "ntfy:" + string(session.State) + ":" + eventIdentity(session)
	if !d.reserveNotification(session.ID, key, "ntfy") {
		return
	}
	priority, tag := "3", "white_check_mark"
	if session.State == StateBlocked {
		priority, tag = "5", "warning"
	} else if session.State == StateError {
		priority, tag = "5", "rotating_light"
	}
	title, body := notificationTitle(session), notificationBody(session)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, _, lastErr = d.runner.Run(callCtx, d.ntfyCommand, "-T", title, "-p", priority, "-g", tag, body)
		cancel()
		if lastErr == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
		}
	}
	d.finishNotification(session.ID, key, lastErr)
	if lastErr != nil {
		d.logger.Printf("ntfy delivery failed session=%s: %v", session.ID, lastErr)
	}
}

func eventIdentity(session *Session) string {
	if session.LastTurnID != "" {
		return session.LastTurnID
	}
	return session.Evidence.Timestamp.Format(time.RFC3339Nano)
}

func (d *Daemon) reserveNotification(sessionID, key, channel string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	session := d.state.Sessions[sessionID]
	if session == nil {
		return false
	}
	if session.Notifications == nil {
		session.Notifications = map[string]NotificationRecord{}
	}
	if _, exists := session.Notifications[key]; exists {
		return false
	}
	session.Notifications[key] = NotificationRecord{Key: key, Channel: channel}
	_ = d.store.Save(d.state)
	return true
}

func (d *Daemon) finishNotification(sessionID, key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session := d.state.Sessions[sessionID]
	if session == nil {
		return
	}
	record := session.Notifications[key]
	if err != nil {
		record.LastError = err.Error()
	} else {
		record.SentAt = time.Now().UTC()
		record.LastError = ""
	}
	session.Notifications[key] = record
	_ = d.store.Save(d.state)
}
