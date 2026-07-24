package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Daemon struct {
	mu          sync.Mutex
	lifeMu      sync.Mutex
	attentionMu sync.Mutex
	captureMu   sync.Mutex
	cleanupMu   sync.Mutex
	wg          sync.WaitGroup

	paths    Paths
	store    *Store
	state    StateData
	config   Config
	runner   CommandRunner
	kitty    KittyClient
	logger   *log.Logger
	sshAgent sshAgentInfo

	ctx           context.Context
	cancel        context.CancelFunc
	listener      net.Listener
	watcher       *net.UnixConn
	conns         map[net.Conn]struct{}
	started       bool
	closed        bool
	workerErr     error
	events        chan WatcherEvent
	capturing     map[string]bool
	cleaning      map[string]bool
	cleanups      map[string]*sync.Mutex
	deleted       map[string]string
	remotes       *RemoteManager
	agentRelays   *agentRelayManager
	attentionSubs map[chan struct{}]struct{}
}

func NewDaemon(paths Paths, runner CommandRunner, logger *log.Logger) (*Daemon, error) {
	if paths.AgentDir == "" && paths.RuntimeDir != "" {
		paths.AgentDir = filepath.Join(paths.RuntimeDir, "agents")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = log.New(os.Stderr, "zkad: ", log.LstdFlags)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	store := NewStore(paths)
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	if state.Node.ID == "" {
		state.Node.ID, err = randomID()
		if err != nil {
			return nil, err
		}
	}
	if state.Node.Name == "" {
		state.Node.Name, _ = os.Hostname()
		if state.Node.Name == "" {
			state.Node.Name = "localhost"
		}
	}
	state.Node.Platform = runtime.GOOS + "/" + runtime.GOARCH
	now := time.Now().UTC()
	for _, workspace := range state.Workspaces {
		normalizeWorkspace(workspace)
		for _, pane := range workspace.Panes {
			if pane.State == StateWorking || pane.State == StateBlocked {
				pane.State = StateUnknown
				pane.Evidence = Evidence{Source: "zkad", Event: "daemon_restart", Detail: "fresh agent evidence required", Timestamp: now}
				pane.UpdatedAt = now
			}
			pane.BackendStart = false
		}
		workspace.RecomputeAttention()
		for _, attachment := range workspace.Attachments {
			// Client readiness is a live assertion made by each zmx attach
			// process. A restarted daemon must wait for a fresh heartbeat.
			attachment.ClientHeartbeats = map[string]time.Time{}
		}
	}
	if err := store.Save(state); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		paths:  paths,
		store:  store,
		state:  state,
		config: cfg,
		runner: runner,
		kitty: KittyClient{
			Runner:  runner,
			Command: cfg.Kitty.KittenCommand,
		},
		logger:        logger,
		sshAgent:      newSSHAgentInfo(cfg, os.Getenv("SSH_AUTH_SOCK")),
		ctx:           ctx,
		cancel:        cancel,
		events:        make(chan WatcherEvent, 128),
		capturing:     map[string]bool{},
		cleaning:      map[string]bool{},
		cleanups:      map[string]*sync.Mutex{},
		deleted:       map[string]string{},
		conns:         map[net.Conn]struct{}{},
		attentionSubs: map[chan struct{}]struct{}{},
	}
	store.SetOnSave(d.signalAttention)
	d.remotes = NewRemoteManager(d)
	d.agentRelays = newAgentRelayManager(paths.AgentDir, d.sshAgent.EffectiveSocket)
	return d, nil
}

// Start owns every long-lived worker and returns only after both local sockets
// are bound. Close is idempotent and Wait joins all workers.
func (d *Daemon) Start() error {
	d.lifeMu.Lock()
	if d.closed {
		d.lifeMu.Unlock()
		return fmt.Errorf("daemon has already been closed")
	}
	if d.started {
		d.lifeMu.Unlock()
		return nil
	}
	ln, err := listenUnix(d.paths.Socket)
	if err != nil {
		d.lifeMu.Unlock()
		return err
	}
	watcher, err := listenUnixgram(d.paths.WatcherSocket)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(d.paths.Socket)
		d.lifeMu.Unlock()
		return err
	}
	if d.config.SSH.ForwardAgent {
		for _, workspace := range d.state.Workspaces {
			if workspace.RemoteHost != "" {
				continue
			}
			if _, err := d.agentRelays.ensure(workspace.ID, workspace.AgentAttachmentID); err != nil {
				_ = watcher.Close()
				_ = ln.Close()
				_ = os.Remove(d.paths.Socket)
				_ = os.Remove(d.paths.WatcherSocket)
				d.agentRelays.close()
				d.lifeMu.Unlock()
				return err
			}
		}
	}
	d.listener = ln
	d.watcher = watcher
	d.started = true
	d.logger.Printf("listening on %s", d.paths.Socket)
	d.startWorkerLocked(func(ctx context.Context) { d.acceptLoop(ctx) })
	d.startWorkerLocked(func(ctx context.Context) { d.watcherReadLoop(ctx) })
	d.startWorkerLocked(func(ctx context.Context) { d.topologyLoop(ctx) })
	d.startWorkerLocked(func(ctx context.Context) { d.backendReconcileLoop(ctx) })
	d.lifeMu.Unlock()
	d.resumeLifecycleCleanup()
	return nil
}

func (d *Daemon) Close() error {
	d.lifeMu.Lock()
	if d.closed {
		d.lifeMu.Unlock()
		return nil
	}
	d.closed = true
	d.cancel()
	if d.listener != nil {
		_ = d.listener.Close()
	}
	if d.watcher != nil {
		_ = d.watcher.Close()
	}
	for conn := range d.conns {
		_ = conn.Close()
	}
	d.remotes.Close()
	d.agentRelays.close()
	d.lifeMu.Unlock()
	d.wg.Wait()
	_ = os.Remove(d.paths.Socket)
	_ = os.Remove(d.paths.WatcherSocket)
	return nil
}

func (d *Daemon) Wait() error {
	d.wg.Wait()
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	return d.workerErr
}

func (d *Daemon) Serve(ctx context.Context) error {
	if err := d.Start(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-d.ctx.Done():
	}
	_ = d.Close()
	return d.Wait()
}

func (d *Daemon) startWorker(work func(context.Context)) bool {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	return d.startWorkerLocked(work)
}

func (d *Daemon) startWorkerLocked(work func(context.Context)) bool {
	if d.closed {
		return false
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		work(d.ctx)
	}()
	return true
}

func (d *Daemon) failWorker(err error) {
	if err == nil {
		return
	}
	d.lifeMu.Lock()
	if d.workerErr == nil {
		d.workerErr = err
		d.logger.Printf("worker failed: %v", err)
		d.cancel()
		if d.listener != nil {
			_ = d.listener.Close()
		}
		if d.watcher != nil {
			_ = d.watcher.Close()
		}
		for conn := range d.conns {
			_ = conn.Close()
		}
	}
	d.lifeMu.Unlock()
}

// Kept private for tests and short-lived asynchronous transitions.
func (d *Daemon) waitBackground() { _ = d.Wait() }

func (d *Daemon) acceptLoop(ctx context.Context) {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			d.failWorker(fmt.Errorf("accept daemon connection: %w", err))
			return
		}
		client := conn
		d.lifeMu.Lock()
		if d.closed {
			d.lifeMu.Unlock()
			_ = client.Close()
			return
		}
		d.conns[client] = struct{}{}
		d.lifeMu.Unlock()
		d.startWorker(func(workerCtx context.Context) { d.handleConn(workerCtx, client) })
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		_ = conn.Close()
		d.lifeMu.Lock()
		delete(d.conns, conn)
		d.lifeMu.Unlock()
	}()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
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
	_ = conn.SetDeadline(time.Time{})
	if req.Op == "watch_attention" {
		d.watchAttention(ctx, conn)
		return
	}
	callDeadline := time.Now().Add(60 * time.Second)
	if req.DeadlineUnixNano > 0 {
		requestDeadline := time.Unix(0, req.DeadlineUnixNano)
		if requestDeadline.Before(callDeadline) {
			callDeadline = requestDeadline
		}
	}
	callCtx, cancel := context.WithDeadline(ctx, callDeadline)
	defer cancel()
	data, err := d.dispatch(callCtx, req.Op, req.Payload)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
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

type PaneSpec struct {
	CWD   string `json:"cwd,omitempty"`
	Title string `json:"title,omitempty"`
}

type createWorkspaceRequest struct {
	Name  string     `json:"name,omitempty"`
	Shell []string   `json:"shell,omitempty"`
	Panes []PaneSpec `json:"panes"`
}

type refRequest struct {
	Ref string `json:"ref"`
}

type workspacePaneRequest struct {
	Workspace string `json:"workspace"`
	Pane      string `json:"pane,omitempty"`
	CWD       string `json:"cwd,omitempty"`
}

type preparePaneResponse struct {
	Workspace *Workspace `json:"workspace"`
	Pane      *Pane      `json:"pane"`
	Create    bool       `json:"create"`
}

type allocatePaneResponse struct {
	Workspace *Workspace `json:"workspace"`
	Pane      *Pane      `json:"pane"`
}

type allocatePaneRequest struct {
	Workspace string `json:"workspace"`
	Key       string `json:"key,omitempty"`
	CWD       string `json:"cwd,omitempty"`
}

type backendReconcileRequest struct {
	Workspace string `json:"workspace,omitempty"`
}

type backendReconcileResponse struct {
	Marked    []string `json:"marked,omitempty"`
	Recovered []string `json:"recovered,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
}

type attachmentRequest struct {
	Workspace  string     `json:"workspace"`
	Attachment Attachment `json:"attachment"`
}

type attachmentRefRequest struct {
	Workspace  string `json:"workspace"`
	Attachment string `json:"attachment"`
}

type attachmentUpdateRequest struct {
	Workspace        string                 `json:"workspace"`
	Attachment       string                 `json:"attachment"`
	ExpectedRevision uint64                 `json:"expected_revision,omitempty"`
	Status           AttachmentStatus       `json:"status,omitempty"`
	Views            map[string]RuntimeView `json:"views,omitempty"`
	Error            string                 `json:"error,omitempty"`
}

type attachmentPaneReadyRequest struct {
	Workspace   string `json:"workspace"`
	Attachment  string `json:"attachment"`
	Pane        string `json:"pane"`
	Ready       bool   `json:"ready"`
	AgentSocket string `json:"agent_socket,omitempty"`
}

type workspaceAgentRequest struct {
	Workspace  string `json:"workspace"`
	Attachment string `json:"attachment,omitempty"`
}

type workspaceAgentStatus struct {
	Enabled             bool     `json:"enabled"`
	State               string   `json:"state"`
	Available           bool     `json:"available"`
	Owner               string   `json:"owner"`
	RelaySocket         string   `json:"relay_socket,omitempty"`
	ClaimedAttachmentID string   `json:"claimed_attachment_id,omitempty"`
	ClaimedNodeID       string   `json:"claimed_node_id,omitempty"`
	LegacyPaneIDs       []string `json:"legacy_pane_ids,omitempty"`
}

type manifestUpdateRequest struct {
	Workspace        string                 `json:"workspace"`
	Attachment       string                 `json:"attachment"`
	ExpectedRevision uint64                 `json:"expected_revision,omitempty"`
	Manifest         Manifest               `json:"manifest"`
	Views            map[string]RuntimeView `json:"views"`
}

type renameWorkspaceRequest struct {
	Workspace        string `json:"workspace"`
	Name             string `json:"name"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`
}

type closePanesRequest struct {
	Workspace        string                 `json:"workspace"`
	Attachment       string                 `json:"attachment"`
	ExpectedRevision uint64                 `json:"expected_revision"`
	Panes            []string               `json:"panes"`
	Manifest         Manifest               `json:"manifest"`
	Views            map[string]RuntimeView `json:"views"`
}

type killWorkspaceRequest struct {
	WorkspaceID string `json:"workspace_id"`
}

type workspaceDeletionResponse struct {
	DeletedWorkspaceID string   `json:"deleted_workspace_id"`
	Name               string   `json:"name"`
	Remaining          []string `json:"remaining,omitempty"`
}

type moveCommitRequest struct {
	Workspace        string `json:"workspace"`
	Destination      string `json:"destination"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type moveCommitResponse struct {
	Workspace *Workspace  `json:"workspace"`
	Previous  *Attachment `json:"previous,omitempty"`
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
		return map[string]any{"pid": os.Getpid(), "schema_version": stateSchemaVersion, "protocol_version": protocolVersion, "node": d.state.Node}, nil
	case "node":
		return d.state.Node, nil
	case "ssh_agent":
		return d.sshAgent, nil
	case "create_workspace":
		var req createWorkspaceRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.createWorkspace(req)
	case "delete_workspace":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return nil, d.deleteWorkspace(req.Ref)
	case "get_workspace":
		var req refRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.getWorkspace(req.Ref)
	case "list_workspaces":
		return d.listWorkspaces(), nil
	case "attention_snapshot":
		return d.attentionSnapshot(), nil
	case "pause_attention":
		return d.setAttentionPaused(true)
	case "resume_attention":
		return d.setAttentionPaused(false)
	case "toggle_attention":
		return d.toggleAttentionPaused()
	case "prepare_pane":
		var req workspacePaneRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.preparePane(req.Workspace, req.Pane, req.CWD)
	case "allocate_pane":
		var req allocatePaneRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.allocatePane(req.Workspace, req.Key, req.CWD)
	case "reconcile_backends":
		var req backendReconcileRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.reconcileBackends(ctx, req.Workspace)
	case "register_attachment":
		var req attachmentRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.registerAttachment(req.Workspace, req.Attachment)
	case "get_attachment":
		var req attachmentRefRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.getAttachment(req.Workspace, req.Attachment)
	case "update_attachment":
		var req attachmentUpdateRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.updateAttachment(req)
	case "set_attachment_pane_ready":
		var req attachmentPaneReadyRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.setAttachmentPaneReady(req)
	case "workspace_agent_claim":
		var req workspaceAgentRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.claimWorkspaceAgent(req)
	case "workspace_agent_release":
		var req workspaceAgentRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.releaseWorkspaceAgent(req.Workspace)
	case "workspace_agent_status":
		var req workspaceAgentRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.workspaceAgentStatus(req.Workspace)
	case "update_manifest":
		var req manifestUpdateRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.updateManifest(req)
	case "rename_workspace":
		var req renameWorkspaceRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.renameWorkspace(req)
	case "close_panes":
		var req closePanesRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.closePanes(ctx, req)
	case "kill_workspace":
		var req killWorkspaceRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.killWorkspace(ctx, req.WorkspaceID)
	case "commit_move":
		var req moveCommitRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.commitMove(req)
	case "detach_attachment":
		var req attachmentRefRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.detachAttachment(req.Workspace, req.Attachment)
	case "event":
		var event Event
		if err := decodePayload(raw, &event); err != nil {
			return nil, err
		}
		return d.applyEvent(ctx, event)
	case "seen":
		var req workspacePaneRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		return d.markSeen(req.Workspace, req.Pane)
	case "remote_call":
		var req remoteDaemonRequest
		if err := decodePayload(raw, &req); err != nil {
			return nil, err
		}
		result, err := d.remotes.Call(ctx, req.Host, req.Op, json.RawMessage(req.Payload))
		return result, withSSHAgentMismatchHint(err, d.sshAgent, req.CallerSSHAuthSock)
	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}
}

func (d *Daemon) createWorkspace(req createWorkspaceRequest) (*Workspace, error) {
	if len(req.Panes) == 0 {
		req.Panes = []PaneSpec{{}}
	}
	if len(req.Shell) == 0 {
		req.Shell = append([]string(nil), d.config.Shell.Command...)
	}
	if len(req.Shell) == 0 || req.Shell[0] == "" {
		return nil, fmt.Errorf("workspace shell must not be empty")
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "workspace-" + shortID(id)
	}
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, existing := range d.state.Workspaces {
		if existing.RemoteHost == "" && existing.Name == req.Name {
			return nil, fmt.Errorf("workspace name %q already exists", req.Name)
		}
	}
	now := time.Now().UTC()
	workspace := &Workspace{
		ID: id, Name: req.Name, Origin: d.state.Node, Revision: 1,
		Shell: append([]string(nil), req.Shell...), Panes: map[string]*Pane{},
		Attachments: map[string]*Attachment{}, Attention: StateUnknown,
		CreatedAt: now, UpdatedAt: now,
	}
	for position, spec := range req.Panes {
		pane, paneErr := newPane(workspace.ID, position, spec, now)
		if paneErr != nil {
			return nil, paneErr
		}
		workspace.Panes[pane.ID] = pane
	}
	workspace.RecomputeAttention()
	if d.config.SSH.ForwardAgent {
		if _, err := d.agentRelays.ensure(workspace.ID, ""); err != nil {
			return nil, err
		}
	}
	d.state.Workspaces[workspace.ID] = workspace
	if err := d.store.Save(d.state); err != nil {
		delete(d.state.Workspaces, workspace.ID)
		d.agentRelays.remove(workspace.ID)
		return nil, err
	}
	return workspace.Clone(), nil
}

func newPane(workspaceID string, position int, spec PaneSpec, now time.Time) (*Pane, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	title := spec.Title
	if title == "" {
		title = "shell"
	}
	return &Pane{
		ID: id, Position: position, Backend: BackendRef{Kind: "zmx", Ref: backendName(workspaceID, id)},
		CWD: spec.CWD, Title: title, State: StateUnknown,
		Visible:       true,
		Evidence:      Evidence{Source: "zka", Event: "pane_created", Timestamp: now},
		Notifications: map[string]NotificationRecord{}, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (d *Daemon) deleteWorkspace(ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(ref)
	if err != nil {
		return err
	}
	if len(workspace.Attachments) != 0 {
		return fmt.Errorf("workspace %q has attachments and cannot be rollback-deleted", workspace.Name)
	}
	for _, pane := range workspace.Panes {
		if pane.BackendCreated || pane.BackendStart {
			return fmt.Errorf("workspace %q has a started backend and cannot be rollback-deleted", workspace.Name)
		}
	}
	delete(d.state.Workspaces, workspace.ID)
	if err := d.store.Save(d.state); err != nil {
		d.state.Workspaces[workspace.ID] = workspace
		return err
	}
	d.agentRelays.remove(workspace.ID)
	return d.store.RemoveWorkspaceSessions(workspace.ID)
}

func (d *Daemon) getWorkspace(ref string) (*Workspace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(ref)
	if err != nil {
		return nil, err
	}
	return workspace.Clone(), nil
}

func (d *Daemon) listWorkspaces() []*Workspace {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]*Workspace, 0, len(d.state.Workspaces))
	for _, workspace := range d.state.Workspaces {
		result = append(result, workspace.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (d *Daemon) resolveWorkspaceLocked(ref string) (*Workspace, error) {
	if workspace := d.state.Workspaces[ref]; workspace != nil {
		return workspace, nil
	}
	var found *Workspace
	for _, workspace := range d.state.Workspaces {
		if workspace.Name == ref || strings.HasPrefix(workspace.ID, ref) {
			if found != nil {
				return nil, fmt.Errorf("workspace reference %q is ambiguous", ref)
			}
			found = workspace
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown workspace %q", ref)
	}
	return found, nil
}

func resolvePaneLocked(workspace *Workspace, ref string) (*Pane, error) {
	if ref == "" {
		if len(workspace.Panes) == 1 {
			for _, pane := range workspace.Panes {
				return pane, nil
			}
		}
		return nil, fmt.Errorf("workspace %q has %d panes; specify --pane", workspace.Name, len(workspace.Panes))
	}
	if pane := workspace.Panes[ref]; pane != nil {
		return pane, nil
	}
	var found *Pane
	for _, pane := range workspace.Panes {
		if strings.HasPrefix(pane.ID, ref) {
			if found != nil {
				return nil, fmt.Errorf("pane reference %q is ambiguous", ref)
			}
			found = pane
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown pane %q", ref)
	}
	return found, nil
}

func (d *Daemon) preparePane(workspaceRef, paneRef, cwd string) (preparePaneResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return preparePaneResponse{}, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return preparePaneResponse{}, err
	}
	var pane *Pane
	if paneRef == "" {
		pane, _, err = allocatePaneLocked(workspace, "", cwd)
		if err != nil {
			return preparePaneResponse{}, err
		}
	} else {
		pane, err = resolvePaneLocked(workspace, paneRef)
		if err != nil {
			return preparePaneResponse{}, err
		}
	}
	create := !pane.BackendCreated && !pane.BackendStart
	if create {
		pane.BackendStart = true
		pane.UpdatedAt = time.Now().UTC()
	}
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		return preparePaneResponse{}, err
	}
	return preparePaneResponse{Workspace: workspace.Clone(), Pane: pane.Clone(), Create: create}, nil
}

func allocatePaneLocked(workspace *Workspace, key, cwd string) (*Pane, bool, error) {
	if key != "" {
		for _, pane := range workspace.Panes {
			if pane.AllocationKey == key {
				if pane.CWD == "" && cwd != "" {
					pane.CWD = cwd
					pane.UpdatedAt = time.Now().UTC()
					workspace.UpdatedAt = pane.UpdatedAt
				}
				return pane, false, nil
			}
		}
	}
	position := 0
	for _, existing := range workspace.Panes {
		if existing.Position >= position {
			position = existing.Position + 1
		}
	}
	pane, err := newPane(workspace.ID, position, PaneSpec{CWD: cwd}, time.Now().UTC())
	if err != nil {
		return nil, false, err
	}
	pane.AllocationKey = key
	workspace.Panes[pane.ID] = pane
	workspace.Revision++
	workspace.UpdatedAt = time.Now().UTC()
	return pane, true, nil
}

func (d *Daemon) allocatePane(workspaceRef, key, cwd string) (allocatePaneResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return allocatePaneResponse{}, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return allocatePaneResponse{}, err
	}
	pane, _, err := allocatePaneLocked(workspace, key, cwd)
	if err != nil {
		return allocatePaneResponse{}, err
	}
	workspace.RecomputeAttention()
	if err := d.store.Save(d.state); err != nil {
		return allocatePaneResponse{}, err
	}
	return allocatePaneResponse{Workspace: workspace.Clone(), Pane: pane.Clone()}, nil
}

func (d *Daemon) registerAttachment(workspaceRef string, attachment Attachment) (*Attachment, error) {
	if attachment.ID == "" || attachment.Endpoint == "" {
		return nil, fmt.Errorf("attachment requires id and endpoint")
	}
	if attachment.Transport.Kind != "local" && attachment.Transport.Kind != "ssh" {
		return nil, fmt.Errorf("unsupported attachment transport %q", attachment.Transport.Kind)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing := workspace.Attachments[attachment.ID]; existing != nil {
		existing.Endpoint = attachment.Endpoint
		existing.PID = attachment.PID
		existing.Transport = attachment.Transport
		existing.Node = attachment.Node
		existing.Revoked = false
		existing.RevocationClosed = false
		existing.Status = AttachmentPreparing
		existing.LastError = ""
		existing.ClientHeartbeats = map[string]time.Time{}
		existing.UpdatedAt = now
		if existing.Views == nil {
			existing.Views = map[string]RuntimeView{}
		}
		if workspace.PrimaryAttachmentID == "" || workspace.PrimaryAttachmentID == existing.ID {
			existing.Role = AttachmentPrimary
			workspace.PrimaryAttachmentID = existing.ID
		} else {
			existing.Role = AttachmentMirror
		}
		workspace.UpdatedAt = now
		if err := d.store.Save(d.state); err != nil {
			return nil, err
		}
		d.agentRelays.clearAttachment(workspace.ID, existing.ID)
		return existing.Clone(), nil
	}
	attachment.Status = AttachmentPreparing
	attachment.Views = map[string]RuntimeView{}
	attachment.ClientHeartbeats = map[string]time.Time{}
	attachment.CreatedAt = now
	attachment.UpdatedAt = now
	if workspace.PrimaryAttachmentID == "" {
		attachment.Role = AttachmentPrimary
		workspace.PrimaryAttachmentID = attachment.ID
	} else {
		attachment.Role = AttachmentMirror
	}
	workspace.Attachments[attachment.ID] = &attachment
	workspace.UpdatedAt = now
	if err := d.store.Save(d.state); err != nil {
		return nil, err
	}
	d.agentRelays.clearAttachment(workspace.ID, attachment.ID)
	return attachment.Clone(), nil
}

func (d *Daemon) getAttachment(workspaceRef, attachmentID string) (*Attachment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return nil, err
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return nil, fmt.Errorf("unknown attachment %q", attachmentID)
	}
	return attachment.Clone(), nil
}

func (d *Daemon) updateAttachment(req attachmentUpdateRequest) (*Workspace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	attachment := workspace.Attachments[req.Attachment]
	if attachment == nil {
		return nil, fmt.Errorf("unknown attachment %q", req.Attachment)
	}
	if attachment.Status == AttachmentDetached || attachment.Revoked {
		return nil, fmt.Errorf("attachment %s is detached or revoked", attachment.ID)
	}
	if req.ExpectedRevision != 0 && req.ExpectedRevision != workspace.Revision {
		return nil, fmt.Errorf("workspace revision changed: have %d, expected %d", workspace.Revision, req.ExpectedRevision)
	}
	if req.Status != "" {
		attachment.Status = req.Status
	}
	if req.Views != nil {
		attachment.Views = cloneViews(req.Views)
	}
	attachment.LastError = req.Error
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	if attachment.Status == AttachmentReady {
		if err := validateAttachmentReady(workspace, attachment); err != nil {
			attachment.Status = AttachmentUnhealthy
			attachment.LastError = err.Error()
			_ = d.store.Save(d.state)
			return nil, err
		}
		attachment.AppliedRevision = workspace.Revision
	}
	if err := d.store.Save(d.state); err != nil {
		return nil, err
	}
	return workspace.Clone(), nil
}

func (d *Daemon) setAttachmentPaneReady(req attachmentPaneReadyRequest) (*Attachment, error) {
	if req.Workspace == "" || req.Attachment == "" || req.Pane == "" {
		return nil, fmt.Errorf("workspace, attachment, and pane are required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	pane := workspace.Panes[req.Pane]
	if pane == nil {
		return nil, fmt.Errorf("unknown pane %q", req.Pane)
	}
	attachment := workspace.Attachments[req.Attachment]
	if attachment == nil {
		return nil, fmt.Errorf("unknown attachment %q", req.Attachment)
	}
	if attachment.Status == AttachmentDetached || attachment.Revoked {
		return nil, fmt.Errorf("attachment %s is detached or revoked", attachment.ID)
	}
	if req.AgentSocket != "" {
		if !d.config.SSH.ForwardAgent {
			return nil, fmt.Errorf("SSH agent forwarding is disabled")
		}
		if attachment.Transport.Kind != "ssh" {
			return nil, fmt.Errorf("forwarded SSH agents require an SSH attachment")
		}
		if !filepath.IsAbs(req.AgentSocket) {
			return nil, fmt.Errorf("forwarded SSH agent is not an absolute Unix socket")
		}
		info, statErr := os.Lstat(req.AgentSocket)
		if statErr != nil || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("forwarded SSH agent is not an available Unix socket")
		}
	}
	if attachment.ClientHeartbeats == nil {
		attachment.ClientHeartbeats = map[string]time.Time{}
	}
	now := time.Now().UTC()
	if req.Ready {
		attachment.ClientHeartbeats[req.Pane] = now
	} else {
		delete(attachment.ClientHeartbeats, req.Pane)
	}
	d.agentRelays.register(workspace.ID, agentSource{
		Attachment: attachment.ID, Pane: req.Pane, Socket: req.AgentSocket, Heartbeat: now,
	}, req.Ready && req.AgentSocket != "" && !pane.BackendDead)
	return attachment.Clone(), nil
}

func liveLegacyPaneIDs(workspace *Workspace) []string {
	var ids []string
	for _, pane := range workspace.Panes {
		if pane.BackendCreated && !pane.BackendDead && !pane.RemovalPending && pane.AgentRelayVersion < agentRelayVersion {
			ids = append(ids, pane.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (d *Daemon) claimWorkspaceAgent(req workspaceAgentRequest) (workspaceAgentStatus, error) {
	if req.Workspace == "" || req.Attachment == "" {
		return workspaceAgentStatus{}, fmt.Errorf("workspace and attachment are required")
	}
	if !d.config.SSH.ForwardAgent {
		return workspaceAgentStatus{}, fmt.Errorf("SSH agent forwarding is disabled; enable services.zka.ssh.forwardAgent")
	}
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	attachment := workspace.Attachments[req.Attachment]
	if attachment == nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("unknown attachment %q", req.Attachment)
	}
	if attachment.Transport.Kind != "ssh" || attachment.Status != AttachmentReady || attachment.Revoked {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("attachment %s is not a ready SSH attachment", attachment.ID)
	}
	legacy := liveLegacyPaneIDs(workspace)
	if len(legacy) != 0 {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("workspace has legacy panes without a stable agent relay: %s; recreate those panes or the workspace", strings.Join(legacy, ", "))
	}
	if !d.agentRelays.sourceAvailable(workspace.ID, attachment.ID) {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("attachment %s has no fresh forwarded SSH agent", attachment.ID)
	}
	workspace.AgentAttachmentID = attachment.ID
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	workspaceID := workspace.ID
	d.mu.Unlock()
	d.agentRelays.setClaim(workspaceID, attachment.ID)
	return d.workspaceAgentStatus(workspaceID)
}

func (d *Daemon) releaseWorkspaceAgent(workspaceRef string) (workspaceAgentStatus, error) {
	if workspaceRef == "" {
		return workspaceAgentStatus{}, fmt.Errorf("workspace is required")
	}
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	workspace.AgentAttachmentID = ""
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	workspaceID := workspace.ID
	d.mu.Unlock()
	d.agentRelays.setClaim(workspaceID, "")
	return d.workspaceAgentStatus(workspaceID)
}

func (d *Daemon) workspaceAgentStatus(workspaceRef string) (workspaceAgentStatus, error) {
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return workspaceAgentStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceAgentStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	status := workspaceAgentStatus{
		Enabled: d.config.SSH.ForwardAgent, Owner: "origin",
		ClaimedAttachmentID: workspace.AgentAttachmentID,
		LegacyPaneIDs:       liveLegacyPaneIDs(workspace),
	}
	if d.config.SSH.ForwardAgent {
		status.RelaySocket = d.agentRelays.path(workspace.ID)
	}
	if workspace.AgentAttachmentID != "" {
		status.Owner = "attachment"
		if attachment := workspace.Attachments[workspace.AgentAttachmentID]; attachment != nil {
			status.ClaimedNodeID = attachment.Node.ID
		}
	}
	workspaceID := workspace.ID
	claimed := workspace.AgentAttachmentID
	d.mu.Unlock()

	switch {
	case !status.Enabled:
		status.State = "disabled"
	case len(status.LegacyPaneIDs) != 0:
		status.State = "legacy"
	case claimed == "":
		status.Available = d.agentRelays.available(workspaceID, "")
		if status.Available {
			status.State = "origin"
		} else {
			status.State = "disconnected"
		}
	default:
		status.Available = d.agentRelays.available(workspaceID, claimed)
		if status.Available {
			status.State = "forwarded"
		} else {
			status.State = "disconnected"
		}
	}
	return status, nil
}

func validateAttachmentReady(workspace *Workspace, attachment *Attachment) error {
	return validateViewsReady(attachment, manifestPaneIDs(workspace))
}

func validateViewsReady(attachment *Attachment, paneIDs map[string]bool) error {
	for paneID := range paneIDs {
		view, ok := attachment.Views[paneID]
		if !ok || !view.Ready || view.WindowID <= 0 {
			return fmt.Errorf("attachment %s is missing ready pane %s", attachment.ID, paneID)
		}
	}
	return nil
}

func cloneViews(views map[string]RuntimeView) map[string]RuntimeView {
	copy := make(map[string]RuntimeView, len(views))
	for id, view := range views {
		copy[id] = view
	}
	return copy
}

func (d *Daemon) updateManifest(req manifestUpdateRequest) (*Workspace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	attachment := workspace.Attachments[req.Attachment]
	if attachment == nil {
		return nil, fmt.Errorf("unknown attachment %q", req.Attachment)
	}
	if attachment.Status == AttachmentDetached || attachment.Revoked {
		return nil, fmt.Errorf("attachment %s is detached or revoked", attachment.ID)
	}
	if req.ExpectedRevision != 0 && req.ExpectedRevision != workspace.Revision {
		return nil, fmt.Errorf("workspace revision changed: have %d, expected %d", workspace.Revision, req.ExpectedRevision)
	}
	if err := validateManifest(workspace, req.Manifest); err != nil {
		attachment.Status = AttachmentUnhealthy
		attachment.LastError = err.Error()
		_ = d.store.Save(d.state)
		return nil, err
	}
	attachment.Views = cloneViews(req.Views)
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	if err := validateViewsReady(attachment, topologyPaneIDs(req.Manifest.Topology)); err != nil {
		attachment.Status = AttachmentUnhealthy
		attachment.LastError = err.Error()
		_ = d.store.Save(d.state)
		return nil, err
	}
	if workspace.RemoteHost != "" || attachment.Role != AttachmentPrimary || workspace.PrimaryAttachmentID != attachment.ID {
		attachment.Status = AttachmentReady
		attachment.AppliedRevision = workspace.Revision
		if err := d.store.Save(d.state); err != nil {
			return nil, err
		}
		return workspace.Clone(), nil
	}
	acceptCapturedCWD := attachment.Transport.Kind != "ssh"
	if !acceptCapturedCWD {
		preserveOriginPaneCWD(workspace, req.Manifest.Topology)
	}
	applyManifestPaneMetadata(workspace, req.Manifest.Topology, acceptCapturedCWD)
	if req.Manifest.Session != workspace.Manifest.Session || !nodesEqual(req.Manifest.Topology, workspace.Manifest.Topology) {
		workspace.Revision++
	}
	req.Manifest.CapturedAt = time.Now().UTC()
	workspace.Manifest = req.Manifest
	workspace.UpdatedAt = req.Manifest.CapturedAt
	attachment.Status = AttachmentReady
	attachment.LastError = ""
	attachment.AppliedRevision = workspace.Revision
	if err := d.store.Save(d.state); err != nil {
		return nil, err
	}
	return workspace.Clone(), nil
}

func applyManifestPaneMetadata(workspace *Workspace, nodes []Node, acceptCWD bool) {
	seen := map[string]bool{}
	var visit func([]Node)
	visit = func(children []Node) {
		for _, node := range children {
			if node.Kind == "pane" {
				if pane := workspace.Panes[node.PaneID]; pane != nil {
					pane.Visible = true
					if node.Title != "" {
						pane.Title = stripStateMarker(node.Title)
					}
					if acceptCWD && node.CWD != "" {
						pane.CWD = node.CWD
					}
					seen[pane.ID] = true
				}
			}
			visit(node.Children)
		}
	}
	visit(nodes)
	for id, pane := range workspace.Panes {
		if !seen[id] {
			pane.Visible = false
		}
	}
}

func preserveOriginPaneCWD(workspace *Workspace, nodes []Node) {
	for i := range nodes {
		node := &nodes[i]
		if node.Kind == "pane" {
			if pane := workspace.Panes[node.PaneID]; pane != nil {
				node.CWD = pane.CWD
			}
		}
		preserveOriginPaneCWD(workspace, node.Children)
	}
}

func nodesEqual(a, b []Node) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

func validateManifest(workspace *Workspace, manifest Manifest) error {
	if strings.TrimSpace(manifest.Session) == "" {
		return fmt.Errorf("manifest session is empty")
	}
	seen := map[string]bool{}
	var visit func([]Node) error
	visit = func(nodes []Node) error {
		for _, node := range nodes {
			if node.Kind == "pane" {
				if workspace.Panes[node.PaneID] == nil {
					return fmt.Errorf("manifest references unknown pane %s", node.PaneID)
				}
				if seen[node.PaneID] {
					return fmt.Errorf("manifest contains pane %s more than once", node.PaneID)
				}
				seen[node.PaneID] = true
			}
			if err := visit(node.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(manifest.Topology); err != nil {
		return err
	}
	if len(seen) == 0 {
		return fmt.Errorf("manifest topology contains no panes")
	}
	return nil
}

func manifestPaneIDs(workspace *Workspace) map[string]bool {
	result := topologyPaneIDs(workspace.Manifest.Topology)
	if len(result) == 0 {
		for id, pane := range workspace.Panes {
			if pane.Visible {
				result[id] = true
			}
		}
	}
	return result
}

func topologyPaneIDs(topology []Node) map[string]bool {
	result := map[string]bool{}
	var visit func([]Node)
	visit = func(nodes []Node) {
		for _, node := range nodes {
			if node.Kind == "pane" && node.PaneID != "" {
				result[node.PaneID] = true
			}
			visit(node.Children)
		}
	}
	visit(topology)
	return result
}

func (d *Daemon) commitMove(req moveCommitRequest) (moveCommitResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		return moveCommitResponse{}, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return moveCommitResponse{}, err
	}
	destination := workspace.Attachments[req.Destination]
	if destination == nil {
		return moveCommitResponse{}, fmt.Errorf("unknown destination attachment %q", req.Destination)
	}
	// A control connection can drop after the origin commits but before the
	// response reaches the destination. Recognize that replay before applying
	// optimistic revision checks so the destination is not torn down after a
	// successful handoff.
	if workspace.PrimaryAttachmentID == destination.ID && destination.Role == AttachmentPrimary {
		return moveCommitResponse{Workspace: workspace.Clone()}, nil
	}
	if workspace.Revision != req.ExpectedRevision {
		return moveCommitResponse{}, fmt.Errorf("workspace revision changed: have %d, expected %d", workspace.Revision, req.ExpectedRevision)
	}
	if destination.Status != AttachmentReady || destination.AppliedRevision != workspace.Revision {
		return moveCommitResponse{}, fmt.Errorf("destination attachment is not ready at revision %d", workspace.Revision)
	}
	if destination.Transport.Kind == "ssh" {
		for paneID := range manifestPaneIDs(workspace) {
			if !clientHeartbeatFresh(destination.ClientHeartbeats[paneID], time.Now().UTC()) {
				return moveCommitResponse{}, fmt.Errorf("destination attachment has no live SSH/zmx client for pane %s", paneID)
			}
		}
	}
	var previous *Attachment
	if old := workspace.Attachments[workspace.PrimaryAttachmentID]; old != nil {
		previous = old.Clone()
		old.Role = AttachmentMirror
		old.Revoked = true
		old.RevocationClosed = false
		workspace.PendingRevocations = appendUnique(workspace.PendingRevocations, old.ID)
		if old.Node.ID == d.state.Node.ID && strings.HasPrefix(old.Endpoint, "unix:") {
			d.scheduleCapture(old.Endpoint)
		}
	}
	destination.Role = AttachmentPrimary
	destination.Revoked = false
	destination.RevocationClosed = false
	workspace.PrimaryAttachmentID = destination.ID
	workspace.Revision++
	destination.AppliedRevision = workspace.Revision
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		return moveCommitResponse{}, err
	}
	if previous != nil {
		d.agentRelays.clearAttachment(workspace.ID, previous.ID)
	}
	return moveCommitResponse{Workspace: workspace.Clone(), Previous: previous}, nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (d *Daemon) detachAttachment(workspaceRef, attachmentID string) (*Workspace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		return nil, err
	}
	attachment := workspace.Attachments[attachmentID]
	if attachment == nil {
		return nil, fmt.Errorf("unknown attachment %q", attachmentID)
	}
	attachment.Status = AttachmentDetached
	attachment.Views = map[string]RuntimeView{}
	attachment.ClientHeartbeats = map[string]time.Time{}
	attachment.UpdatedAt = time.Now().UTC()
	workspace.UpdatedAt = attachment.UpdatedAt
	if workspace.PrimaryAttachmentID == attachmentID && workspace.RemoteHost == "" {
		workspace.PrimaryAttachmentID = ""
		attachment.Role = AttachmentMirror
		workspace.Revision++
	}
	workspace.PendingRevocations = removeString(workspace.PendingRevocations, attachmentID)
	if err := d.store.Save(d.state); err != nil {
		return nil, err
	}
	d.agentRelays.clearAttachment(workspace.ID, attachmentID)
	return workspace.Clone(), nil
}

func removeString(values []string, value string) []string {
	result := values[:0]
	for _, existing := range values {
		if existing != value {
			result = append(result, existing)
		}
	}
	return result
}

func (d *Daemon) applyEvent(_ context.Context, event Event) (*Workspace, error) {
	if event.WorkspaceID == "" || event.PaneID == "" || event.Kind == "" {
		return nil, fmt.Errorf("event requires workspace_id, pane_id, and kind")
	}
	now := time.Now().UTC()
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(event.WorkspaceID)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	pane, err := resolvePaneLocked(workspace, event.PaneID)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	before := pane.State
	pane.Evidence = Evidence{Source: event.Source, Event: event.Kind, Detail: event.Detail, TurnID: event.TurnID, Timestamp: now}
	switch event.Source {
	case "codex-hook":
		pane.Agent = "codex"
	case "claude-hook":
		pane.Agent = "claude"
	}
	if event.TurnID != "" {
		pane.LastTurnID = event.TurnID
	}
	pane.AttentionSeen = ""
	switch event.Kind {
	case "session_start":
		pane.State = StateIdle
	case "user_prompt", "post_tool":
		pane.State = StateWorking
	case "permission_request":
		pane.State = StateBlocked
	case "stop":
		pane.State = StateDone
	case "agent_error":
		pane.State = StateError
	case "session_end":
		pane.Agent, pane.State, pane.LastTurnID = "", StateUnknown, ""
	case "process_started":
		pane.Process = ProcessStatus{Running: true, PID: event.PID, Started: now}
		pane.BackendCreated, pane.BackendReady, pane.BackendStart = true, true, false
		pane.AgentRelayVersion = event.AgentRelayVersion
		pane.BackendDead, pane.BackendError = false, ""
	case "process_exit":
		pane.Process.Running, pane.Process.PID, pane.Process.ExitCode, pane.Process.Exited = false, 0, event.ExitCode, now
		pane.BackendReady, pane.BackendStart = false, false
		pane.BackendDead, pane.BackendError = true, event.Detail
		if pane.BackendError == "" {
			pane.BackendError = "backend process exited"
		}
		if event.ExitCode != nil && *event.ExitCode != 0 {
			pane.State = StateError
		} else if pane.State != StateDone {
			pane.State = StateUnknown
		}
	case "backend_error":
		pane.BackendReady, pane.BackendStart, pane.State = false, false, StateError
		pane.BackendDead, pane.BackendError = true, event.Detail
	default:
		d.mu.Unlock()
		return nil, fmt.Errorf("unsupported event kind %q", event.Kind)
	}
	pane.UpdatedAt, workspace.UpdatedAt = now, now
	workspace.RecomputeAttention()
	after := pane.State
	copy := workspace.Clone()
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.mu.Unlock()
	if before != after {
		d.startWorker(func(ctx context.Context) { d.afterTransition(ctx, before, copy, event.PaneID) })
	}
	return copy, nil
}

func (d *Daemon) markSeen(workspaceRef, paneRef string) (*Workspace, error) {
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	var panes []*Pane
	if paneRef != "" {
		pane, paneErr := resolvePaneLocked(workspace, paneRef)
		if paneErr != nil {
			d.mu.Unlock()
			return nil, paneErr
		}
		panes = []*Pane{pane}
	} else {
		for _, pane := range workspace.Panes {
			panes = append(panes, pane)
		}
	}
	changed := false
	for _, pane := range panes {
		switch pane.State {
		case StateDone:
			pane.State = StateIdle
			pane.AttentionSeen = ""
			pane.Evidence = Evidence{Source: "zka", Event: "seen", Timestamp: time.Now().UTC()}
			pane.UpdatedAt = pane.Evidence.Timestamp
			changed = true
		case StateBlocked, StateError:
			identity := attentionEventIdentity(pane)
			if pane.AttentionSeen != identity {
				pane.AttentionSeen = identity
				changed = true
			}
		}
	}
	workspace.RecomputeAttention()
	copy := workspace.Clone()
	err = d.store.Save(d.state)
	d.mu.Unlock()
	if err == nil && changed {
		d.startWorker(func(ctx context.Context) { d.closeDesktopNotifications(ctx, copy, paneRef) })
		d.startWorker(func(ctx context.Context) { d.updateKittyState(ctx, copy) })
	}
	return copy, err
}
