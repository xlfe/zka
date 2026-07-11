package zka

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Daemon struct {
	mu          sync.Mutex
	paths       Paths
	store       *Store
	state       StateData
	runner      CommandRunner
	kitty       KittyClient
	ntfyCommand string
	logger      *log.Logger
}

func NewDaemon(paths Paths, runner CommandRunner, logger *log.Logger) (*Daemon, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = log.New(os.Stderr, "zkad: ", log.LstdFlags)
	}
	store := NewStore(paths)
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, session := range state.Sessions {
		if session.Views == nil {
			session.Views = map[string]View{}
		}
		if session.Notifications == nil {
			session.Notifications = map[string]NotificationRecord{}
		}
		if session.State == StateWorking || session.State == StateBlocked {
			session.State = StateUnknown
			session.Evidence = Evidence{Source: "zkad", Event: "daemon_restart", Detail: "fresh agent evidence required", Timestamp: now}
			session.UpdatedAt = now
		}
		session.BackendStart = false
	}
	if err := store.Save(state); err != nil {
		return nil, err
	}
	ntfyCommand := os.Getenv("ZKA_NTFY_COMMAND")
	if ntfyCommand == "" {
		ntfyCommand = "ntfy-send"
	}
	kittyCommand := os.Getenv("ZKA_KITTEN_COMMAND")
	if kittyCommand == "" {
		kittyCommand = "kitten"
	}
	return &Daemon{
		paths:       paths,
		store:       store,
		state:       state,
		runner:      runner,
		kitty:       KittyClient{Runner: runner, Command: kittyCommand},
		ntfyCommand: ntfyCommand,
		logger:      logger,
	}, nil
}

func (d *Daemon) Serve(ctx context.Context) error {
	ln, err := listenUnix(d.paths.Socket)
	if err != nil {
		return err
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(d.paths.Socket)
	}()
	d.logger.Printf("listening on %s", d.paths.Socket)
	go d.reconcileLoop(ctx)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept daemon connection: %w", err)
		}
		go d.handleConn(ctx, conn)
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(io.LimitReader(conn, maxProtocolMessage))
	var req request
	if err := dec.Decode(&req); err != nil {
		d.writeResponse(conn, nil, fmt.Errorf("decode request: %w", err))
		return
	}
	if req.Version != protocolVersion {
		d.writeResponse(conn, nil, fmt.Errorf("unsupported protocol %d", req.Version))
		return
	}
	data, err := d.dispatch(ctx, req.Op, req.Payload)
	d.writeResponse(conn, data, err)
}

func (d *Daemon) writeResponse(w io.Writer, data any, callErr error) {
	res := response{Version: protocolVersion, OK: callErr == nil}
	if callErr != nil {
		res.Error = callErr.Error()
	} else if data != nil {
		res.Data, _ = json.Marshal(data)
	}
	_ = json.NewEncoder(w).Encode(res)
}

type createSessionRequest struct {
	Name        string   `json:"name"`
	BackendKind string   `json:"backend_kind"`
	Agent       string   `json:"agent"`
	Command     []string `json:"command"`
	CWD         string   `json:"cwd"`
}

type refRequest struct {
	Ref string `json:"ref"`
}

type registerViewRequest struct {
	SessionID string `json:"session_id"`
	View      View   `json:"view"`
}

type prepareViewResponse struct {
	Session *Session `json:"session"`
	Create  bool     `json:"create"`
}

func decodePayload(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing request payload")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid request payload: %w", err)
	}
	return nil
}

func (d *Daemon) dispatch(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	switch op {
	case "ping":
		return map[string]any{"pid": os.Getpid(), "schema_version": stateSchemaVersion}, nil
	case "create_session":
		var req createSessionRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.createSession(req)
	case "delete_session":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return nil, d.deleteSession(req.Ref)
	case "get_session":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.getSession(req.Ref)
	case "list_sessions":
		return d.listSessions(), nil
	case "all_sessions":
		return d.allSessions(), nil
	case "register_view":
		var req registerViewRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.registerView(req.SessionID, req.View)
	case "prepare_view":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.prepareView(req.Ref)
	case "event":
		var event Event
		if err := decodePayload(raw, &event); err != nil {
			return nil, err
		}
		return d.applyEvent(ctx, event)
	case "seen":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.markSeen(ctx, req.Ref)
	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}
}

func (d *Daemon) createSession(req createSessionRequest) (*Session, error) {
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	if len(req.Command) == 0 {
		return nil, fmt.Errorf("agent command must not be empty")
	}
	if req.BackendKind == "" {
		req.BackendKind = "zmx"
	}
	if req.BackendKind != "zmx" {
		return nil, fmt.Errorf("unsupported backend %q", req.BackendKind)
	}
	if req.Agent == "" {
		req.Agent = inferAgent(req.Command[0])
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, existing := range d.state.Sessions {
		if existing.Name == req.Name {
			return nil, fmt.Errorf("session name %q already exists", req.Name)
		}
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	session := &Session{
		ID:            id,
		Name:          req.Name,
		Backend:       BackendRef{Kind: req.BackendKind, Ref: backendName(req.Name, id)},
		Agent:         req.Agent,
		Command:       append([]string(nil), req.Command...),
		CWD:           req.CWD,
		State:         StateUnknown,
		Evidence:      Evidence{Source: "zka", Event: "session_created", Timestamp: now},
		Views:         map[string]View{},
		Notifications: map[string]NotificationRecord{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	d.state.Sessions[id] = session
	if err := d.store.Save(d.state); err != nil {
		delete(d.state.Sessions, id)
		return nil, err
	}
	return session.Clone(), nil
}

func (d *Daemon) deleteSession(ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, err := d.resolveLocked(ref)
	if err != nil {
		return err
	}
	delete(d.state.Sessions, session.ID)
	return d.store.Save(d.state)
}

func (d *Daemon) getSession(ref string) (*Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, err := d.resolveLocked(ref)
	if err != nil {
		return nil, err
	}
	return session.Clone(), nil
}

func (d *Daemon) listSessions() []*Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]*Session, 0, len(d.state.Sessions))
	for _, session := range d.state.Sessions {
		result = append(result, session.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (d *Daemon) allSessions() map[string]*Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make(map[string]*Session, len(d.state.Sessions))
	for id, session := range d.state.Sessions {
		result[id] = session.Clone()
	}
	return result
}

func (d *Daemon) resolveLocked(ref string) (*Session, error) {
	if session, ok := d.state.Sessions[ref]; ok {
		return session, nil
	}
	var found *Session
	for _, session := range d.state.Sessions {
		if session.Name == ref || strings.HasPrefix(session.ID, ref) {
			if found != nil {
				return nil, fmt.Errorf("session reference %q is ambiguous", ref)
			}
			found = session
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown session %q", ref)
	}
	return found, nil
}

func (d *Daemon) registerView(sessionID string, view View) (*Session, error) {
	if view.Endpoint == "" || view.WindowID <= 0 {
		return nil, fmt.Errorf("view requires a kitty endpoint and positive window id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	session, err := d.resolveLocked(sessionID)
	if err != nil {
		return nil, err
	}
	view.Attached = true
	view.LastSeen = time.Now().UTC()
	if session.Views == nil {
		session.Views = map[string]View{}
	}
	session.Views[view.Key()] = view
	session.UpdatedAt = view.LastSeen
	if err := d.store.Save(d.state); err != nil {
		return nil, err
	}
	return session.Clone(), nil
}

func (d *Daemon) prepareView(ref string) (prepareViewResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, err := d.resolveLocked(ref)
	if err != nil {
		return prepareViewResponse{}, err
	}
	if session.Backend.Kind != "zmx" {
		return prepareViewResponse{}, fmt.Errorf("backend adapter %q is modeled but not implemented", session.Backend.Kind)
	}
	create := !session.BackendCreated && !session.BackendStart
	if create {
		session.BackendStart = true
		session.UpdatedAt = time.Now().UTC()
		if err := d.store.Save(d.state); err != nil {
			return prepareViewResponse{}, err
		}
	}
	return prepareViewResponse{Session: session.Clone(), Create: create}, nil
}

func (d *Daemon) applyEvent(ctx context.Context, event Event) (*Session, error) {
	if event.SessionID == "" || event.Kind == "" {
		return nil, fmt.Errorf("event requires session_id and kind")
	}
	now := time.Now().UTC()
	d.mu.Lock()
	session, err := d.resolveLocked(event.SessionID)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	before := session.State
	if event.View != nil && event.View.Endpoint != "" && event.View.WindowID > 0 {
		view := *event.View
		view.Attached = true
		view.LastSeen = now
		if session.Views == nil {
			session.Views = map[string]View{}
		}
		session.Views[view.Key()] = view
	}
	session.Evidence = Evidence{Source: event.Source, Event: event.Kind, Detail: event.Detail, TurnID: event.TurnID, Timestamp: now}
	if event.TurnID != "" {
		session.LastTurnID = event.TurnID
	}
	switch event.Kind {
	case "session_start":
		session.State = StateIdle
	case "user_prompt", "post_tool":
		session.State = StateWorking
	case "permission_request":
		session.State = StateBlocked
	case "stop":
		// Resolve focused-vs-unseen asynchronously against kitty. Using cached
		// focus here can suppress a completion that arrived between polls.
		session.State = StateDone
	case "process_started":
		session.Process = ProcessStatus{Running: true, PID: event.PID, Started: now}
		session.BackendCreated = true
		session.BackendReady = true
		session.BackendStart = false
	case "process_exit":
		session.Process.Running = false
		session.Process.PID = 0
		session.Process.ExitCode = event.ExitCode
		session.Process.Exited = now
		session.BackendReady = false
		session.BackendStart = false
		if event.ExitCode != nil && *event.ExitCode != 0 {
			session.State = StateError
		} else if session.State != StateDone {
			session.State = StateUnknown
		}
	case "backend_error":
		session.BackendReady = false
		session.BackendStart = false
		session.State = StateError
	case "view_attached":
		// View registration above is the entire state change.
	default:
		d.mu.Unlock()
		return nil, fmt.Errorf("unsupported event kind %q", event.Kind)
	}
	session.UpdatedAt = now
	changed := before != session.State
	copy := session.Clone()
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.mu.Unlock()
	if changed {
		go d.afterTransition(context.WithoutCancel(ctx), before, copy)
	}
	return copy, nil
}

func (d *Daemon) markSeen(ctx context.Context, ref string) (*Session, error) {
	d.mu.Lock()
	session, err := d.resolveLocked(ref)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	changed := session.State == StateDone
	if changed {
		session.State = StateIdle
		session.Evidence = Evidence{Source: "zka", Event: "seen", Timestamp: time.Now().UTC()}
		session.UpdatedAt = session.Evidence.Timestamp
	}
	copy := session.Clone()
	err = d.store.Save(d.state)
	d.mu.Unlock()
	if err == nil && changed {
		go d.closeDesktopNotifications(context.WithoutCancel(ctx), copy)
		go d.updateKittyState(context.WithoutCancel(ctx), copy)
	}
	return copy, err
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func backendName(name, id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() == 0 || !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "session"
	}
	return "zka-" + slug + "-" + id[:8]
}

func inferAgent(command string) string {
	base := command
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "codex":
		return "codex"
	default:
		return "generic"
	}
}
