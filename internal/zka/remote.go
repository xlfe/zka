package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type remoteEnvelope struct {
	Protocol     string          `json:"protocol"`
	Version      int             `json:"version"`
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	Op           string          `json:"op,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type remoteDaemonRequest struct {
	Host              string          `json:"host"`
	Op                string          `json:"op"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	CallerSSHAuthSock string          `json:"caller_ssh_auth_sock,omitempty"`
}

type paneReadinessRequest struct {
	Workspace  string `json:"workspace"`
	Attachment string `json:"attachment"`
	Pane       string `json:"pane"`
}

type paneReadinessResponse struct {
	BackendReady bool `json:"backend_ready"`
	BackendDead  bool `json:"backend_dead,omitempty"`
	ClientReady  bool `json:"client_ready"`
}

type RemoteManager struct {
	daemon  *Daemon
	mu      sync.Mutex
	clients map[string]*remoteClient
	closed  bool
}

func NewRemoteManager(daemon *Daemon) *RemoteManager {
	return &RemoteManager{daemon: daemon, clients: map[string]*remoteClient{}}
}

func (m *RemoteManager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, client := range m.clients {
		client.stop()
	}
	m.mu.Unlock()
}

func (m *RemoteManager) client(host string) (*remoteClient, error) {
	if err := validateSSHHost(host); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	if client := m.clients[host]; client != nil {
		client.mu.Lock()
		client.activeCalls++
		client.mu.Unlock()
		m.mu.Unlock()
		return client, nil
	}
	client := newRemoteClient(m, host)
	client.activeCalls = 1
	m.clients[host] = client
	m.mu.Unlock()
	m.daemon.startWorker(func(ctx context.Context) { client.supervise(ctx) })
	return client, nil
}

func (m *RemoteManager) Call(ctx context.Context, host, op string, payload any) (result json.RawMessage, resultErr error) {
	client, err := m.client(host)
	if err != nil {
		return nil, err
	}
	defer func() { m.releaseClient(host, client, resultErr) }()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode remote request: %w", err)
	}
	for {
		result, callErr := client.call(ctx, op, raw)
		if !errors.Is(callErr, errRemoteDisconnected) {
			if callErr == nil {
				m.cacheResult(host, op, result)
			}
			return result, callErr
		}
		select {
		case <-ctx.Done():
			return nil, client.contextError(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *RemoteManager) releaseClient(host string, client *remoteClient, callErr error) {
	m.mu.Lock()
	client.mu.Lock()
	client.activeCalls--
	terminalFailure := client.terminal != nil && errors.Is(callErr, client.terminal)
	abandonInitial := client.activeCalls == 0 && !client.everConnected &&
		(errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded))
	if m.clients[host] == client && (terminalFailure || abandonInitial) {
		delete(m.clients, host)
	}
	client.mu.Unlock()
	m.mu.Unlock()
	if abandonInitial {
		client.stop()
	}
}

func (m *RemoteManager) cacheResult(host, op string, result json.RawMessage) {
	switch op {
	case "list":
		var workspaces []*Workspace
		if json.Unmarshal(result, &workspaces) == nil {
			m.daemon.cacheRemoteSnapshot(host, workspaces)
		}
	case "get", "update_attachment", "update_manifest", "detach_attachment", "seen", "rename_workspace", "close_panes":
		var workspace Workspace
		if json.Unmarshal(result, &workspace) == nil {
			m.daemon.cacheRemoteWorkspace(host, &workspace)
		}
	case "commit_move":
		var response moveCommitResponse
		if json.Unmarshal(result, &response) == nil {
			m.daemon.cacheRemoteWorkspace(host, response.Workspace)
		}
	case "allocate_pane":
		var response allocatePaneResponse
		if json.Unmarshal(result, &response) == nil {
			m.daemon.cacheRemoteWorkspace(host, response.Workspace)
		}
	case "kill_workspace":
		var response workspaceDeletionResponse
		if json.Unmarshal(result, &response) == nil && response.DeletedWorkspaceID != "" {
			m.daemon.evictRemoteWorkspace(host, response.DeletedWorkspaceID)
		}
	}
}

func (m *RemoteManager) cacheEvent(host, op string, payload json.RawMessage) {
	switch op {
	case "snapshot":
		var workspaces []*Workspace
		if err := json.Unmarshal(payload, &workspaces); err != nil {
			m.daemon.logger.Printf("decode remote workspace snapshot from %s: %v", host, err)
			return
		}
		m.daemon.cacheRemoteSnapshot(host, workspaces)
	case "workspace":
		var workspace Workspace
		if err := json.Unmarshal(payload, &workspace); err != nil {
			m.daemon.logger.Printf("decode remote workspace event from %s: %v", host, err)
			return
		}
		m.daemon.cacheRemoteWorkspace(host, &workspace)
	case "deleted_workspace":
		var response workspaceDeletionResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			m.daemon.logger.Printf("decode remote workspace deletion from %s: %v", host, err)
			return
		}
		m.daemon.evictRemoteWorkspace(host, response.DeletedWorkspaceID)
	}
}

var errRemoteDisconnected = errors.New("remote SSH control connection disconnected")

const maxSSHStderr = 8 << 10

type boundedTailBuffer struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= maxSSHStderr {
		b.data = append(b.data[:0], p[len(p)-maxSSHStderr:]...)
		b.truncated = true
		return written, nil
	}
	if overflow := len(b.data) + len(p) - maxSSHStderr; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *boundedTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	detail := string(b.data)
	if b.truncated {
		return fmt.Sprintf("[SSH stderr truncated; showing last %d bytes]\n%s", maxSSHStderr, detail)
	}
	return detail
}

type remoteClient struct {
	manager *RemoteManager
	host    string

	mu            sync.Mutex
	writeMu       sync.Mutex
	stdin         io.WriteCloser
	encoder       *json.Encoder
	process       *exec.Cmd
	connected     bool
	everConnected bool
	activeCalls   int
	terminal      error
	lastFailure   error
	stateCh       chan struct{}
	pending       map[string]chan remoteEnvelope
	sequence      atomic.Uint64
	stopCh        chan struct{}
	stopOnce      sync.Once
}

func newRemoteClient(manager *RemoteManager, host string) *remoteClient {
	return &remoteClient{manager: manager, host: host, stateCh: make(chan struct{}), pending: map[string]chan remoteEnvelope{}, stopCh: make(chan struct{})}
}

func (c *remoteClient) stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.mu.Lock()
		if c.process != nil && c.process.Process != nil {
			_ = c.process.Process.Kill()
		}
		c.mu.Unlock()
	})
}

func (c *remoteClient) signalStateLocked() {
	close(c.stateCh)
	c.stateCh = make(chan struct{})
}

func (c *remoteClient) supervise(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		default:
		}
		cmd, stdin, stdout, stderr, err := c.startSSH(ctx)
		if err != nil {
			c.setTerminal(fmt.Errorf("start SSH control connection to %s: %w", c.host, err))
			return
		}
		c.mu.Lock()
		c.process, c.stdin, c.encoder = cmd, stdin, json.NewEncoder(stdin)
		c.mu.Unlock()
		readerDone := make(chan struct{})
		if !c.manager.daemon.startWorker(func(workerCtx context.Context) {
			defer close(readerDone)
			c.readLoop(workerCtx, stdout)
		}) {
			close(readerDone)
		}
		waitErr := cmd.Wait()
		<-readerDone
		c.mu.Lock()
		wasConnected := c.connected
		everConnected := c.everConnected
		c.mu.Unlock()
		failure := sshConnectionError(c.host, waitErr, stderr.String())
		c.disconnected(failure)
		if wasConnected {
			backoff = 250 * time.Millisecond
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-c.stopCh:
			return
		default:
		}
		c.manager.daemon.logger.Printf("%v", failure)
		if sshExitCode(waitErr) != 255 {
			c.setTerminal(failure)
			return
		}
		if !everConnected {
			c.setTerminal(failure)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (c *remoteClient) startSSH(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, *boundedTailBuffer, error) {
	args := append([]string(nil), c.manager.daemon.config.SSH.Options...)
	args = append(args, "-T", "--", c.host, "exec", "zka", "remote-control")
	cmd := exec.CommandContext(ctx, c.manager.daemon.config.SSH.Command, args...)
	stderr := &boundedTailBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, nil, err
	}
	return cmd, stdin, stdout, stderr, nil
}

func (c *remoteClient) readLoop(ctx context.Context, input io.Reader) {
	reader := bufio.NewReaderSize(input, 64<<10)
	first := true
	for {
		message, err := readRemoteEnvelope(reader)
		if err != nil {
			return
		}
		if message.Protocol != remoteProtocolName || message.Version != protocolVersion {
			c.setTerminal(fmt.Errorf("remote %s uses incompatible protocol %q version %d", c.host, message.Protocol, message.Version))
			c.mu.Lock()
			if c.process != nil && c.process.Process != nil {
				_ = c.process.Process.Kill()
			}
			c.mu.Unlock()
			return
		}
		if first {
			first = false
			if message.Type != "hello" {
				c.setTerminal(fmt.Errorf("remote %s did not send a hello", c.host))
				return
			}
			c.mu.Lock()
			c.connected = true
			c.everConnected = true
			c.terminal = nil
			c.lastFailure = nil
			c.signalStateLocked()
			c.mu.Unlock()
			continue
		}
		switch message.Type {
		case "response":
			c.mu.Lock()
			waiter := c.pending[message.ID]
			delete(c.pending, message.ID)
			c.mu.Unlock()
			if waiter != nil {
				waiter <- message
			}
		case "event":
			c.manager.cacheEvent(c.host, message.Op, message.Payload)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (c *remoteClient) disconnected(cause error) {
	c.mu.Lock()
	c.connected = false
	c.lastFailure = cause
	c.process, c.stdin, c.encoder = nil, nil, nil
	for id, waiter := range c.pending {
		delete(c.pending, id)
		waiter <- remoteEnvelope{Error: errRemoteDisconnected.Error()}
	}
	c.signalStateLocked()
	c.mu.Unlock()
}

func (c *remoteClient) setTerminal(err error) {
	c.mu.Lock()
	c.terminal = err
	c.lastFailure = err
	c.connected = false
	c.signalStateLocked()
	c.mu.Unlock()
}

func (c *remoteClient) call(ctx context.Context, op string, payload json.RawMessage) (json.RawMessage, error) {
	for {
		c.mu.Lock()
		if c.terminal != nil {
			err := c.terminal
			c.mu.Unlock()
			return nil, err
		}
		if c.connected && c.encoder != nil {
			id := fmt.Sprintf("%d", c.sequence.Add(1))
			waiter := make(chan remoteEnvelope, 1)
			c.pending[id] = waiter
			encoder := c.encoder
			c.mu.Unlock()
			message := remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "request", ID: id, Op: op, Payload: payload}
			c.writeMu.Lock()
			err := encoder.Encode(message)
			c.writeMu.Unlock()
			if err != nil {
				c.mu.Lock()
				delete(c.pending, id)
				c.mu.Unlock()
				return nil, errRemoteDisconnected
			}
			select {
			case response := <-waiter:
				if response.Error == errRemoteDisconnected.Error() {
					return nil, errRemoteDisconnected
				}
				if response.Error != "" {
					return nil, errors.New(response.Error)
				}
				return response.Payload, nil
			case <-ctx.Done():
				c.mu.Lock()
				delete(c.pending, id)
				c.mu.Unlock()
				return nil, c.contextError(ctx.Err())
			case <-c.stopCh:
				return nil, context.Canceled
			}
		}
		stateCh := c.stateCh
		c.mu.Unlock()
		select {
		case <-stateCh:
		case <-ctx.Done():
			return nil, c.contextError(ctx.Err())
		case <-c.stopCh:
			return nil, context.Canceled
		}
	}
}

func (c *remoteClient) contextError(cause error) error {
	c.mu.Lock()
	lastFailure := c.lastFailure
	c.mu.Unlock()
	if lastFailure == nil {
		return cause
	}
	return fmt.Errorf("remote SSH control connection to %s: %w; last failure: %v", c.host, cause, lastFailure)
}

func sshConnectionError(host string, waitErr error, stderr string) error {
	status := sshExitCode(waitErr)
	summary := fmt.Sprintf("SSH control connection to %s exited", host)
	if status >= 0 {
		summary = fmt.Sprintf("%s with status %d", summary, status)
	} else if waitErr != nil {
		summary = fmt.Sprintf("%s: %v", summary, waitErr)
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("%s: %s", summary, detail)
	}
	return errors.New(summary)
}

func sshExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func validateSSHHost(host string) error {
	if strings.TrimSpace(host) == "" || strings.HasPrefix(host, "-") {
		return fmt.Errorf("invalid SSH host alias %q", host)
	}
	for _, r := range host {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@%", r) {
			continue
		}
		return fmt.Errorf("invalid SSH host alias %q", host)
	}
	return nil
}

func readRemoteEnvelope(reader *bufio.Reader) (remoteEnvelope, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > remoteProtocolMax {
			return remoteEnvelope{}, fmt.Errorf("remote protocol message exceeds %d bytes", remoteProtocolMax)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return remoteEnvelope{}, err
	}
	var message remoteEnvelope
	if err := json.Unmarshal(line, &message); err != nil {
		return remoteEnvelope{}, fmt.Errorf("decode remote protocol message: %w", err)
	}
	return message, nil
}

type remoteControlWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *remoteControlWriter) send(message remoteEnvelope) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(message)
}

func runRemoteControl(ctx context.Context, paths Paths, stdin io.Reader, stdout io.Writer) error {
	writer := &remoteControlWriter{enc: json.NewEncoder(stdout)}
	if err := writer.send(remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "hello", Capabilities: []string{"workspace-snapshots", "events", "two-phase-move", "revocation", "workspace-lifecycle"}}); err != nil {
		return err
	}
	api := NewAPI(paths)
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		streamRemoteEvents(watchCtx, api, writer)
	}()
	reader := bufio.NewReaderSize(stdin, 64<<10)
	for {
		message, err := readRemoteEnvelope(reader)
		if errors.Is(err, io.EOF) {
			cancel()
			<-watchDone
			return nil
		}
		if err != nil {
			cancel()
			<-watchDone
			return err
		}
		response := remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "response", ID: message.ID}
		if message.Protocol != remoteProtocolName || message.Version != protocolVersion || message.Type != "request" {
			response.Error = "incompatible remote request"
		} else {
			response.Payload, err = dispatchRemoteControl(ctx, api, message.Op, message.Payload)
			if err != nil {
				response.Error = err.Error()
				response.Payload = nil
			}
		}
		if err := writer.send(response); err != nil {
			cancel()
			<-watchDone
			return err
		}
	}
}

func streamRemoteEvents(ctx context.Context, api API, writer *remoteControlWriter) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	seen := map[string]string{}
	first := true
	for {
		workspaces, err := authoritativeWorkspaces(ctx, api)
		if err == nil {
			if first {
				payload, _ := json.Marshal(workspaces)
				if writer.send(remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "event", Op: "snapshot", Payload: payload}) != nil {
					return
				}
				first = false
			}
			current := map[string]bool{}
			for _, workspace := range workspaces {
				current[workspace.ID] = true
				fingerprint := fmt.Sprintf("%d:%s:%s", workspace.Revision, workspace.UpdatedAt.Format(time.RFC3339Nano), workspace.Attention)
				if seen[workspace.ID] == fingerprint {
					continue
				}
				seen[workspace.ID] = fingerprint
				payload, _ := json.Marshal(workspace)
				if writer.send(remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "event", Op: "workspace", Payload: payload}) != nil {
					return
				}
			}
			for id := range seen {
				if current[id] {
					continue
				}
				delete(seen, id)
				payload, _ := json.Marshal(workspaceDeletionResponse{DeletedWorkspaceID: id})
				if writer.send(remoteEnvelope{Protocol: remoteProtocolName, Version: protocolVersion, Type: "event", Op: "deleted_workspace", Payload: payload}) != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func dispatchRemoteControl(ctx context.Context, api API, op string, raw json.RawMessage) (json.RawMessage, error) {
	var value any
	switch op {
	case "list":
		workspaces, err := authoritativeWorkspaces(ctx, api)
		value = workspaces
		if err != nil {
			return nil, err
		}
	case "get":
		var req refRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		workspace, err := api.Workspace(ctx, req.Ref)
		if err == nil && workspace.RemoteHost != "" {
			err = fmt.Errorf("workspace %q is not authoritative on this host", req.Ref)
		}
		value = workspace
		if err != nil {
			return nil, err
		}
	case "register_attachment":
		var req attachmentRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		attachment, err := api.RegisterAttachment(ctx, req.Workspace, req.Attachment)
		value = attachment
		if err != nil {
			return nil, err
		}
	case "allocate_pane":
		var req allocatePaneRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		result, err := api.AllocatePane(ctx, req.Workspace, req.Key, req.CWD)
		value = result
		if err != nil {
			return nil, err
		}
	case "reconcile_backends":
		var req backendReconcileRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		result, err := api.ReconcileBackends(ctx, req.Workspace)
		value = result
		if err != nil {
			return nil, err
		}
	case "update_attachment":
		var req attachmentUpdateRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.UpdateAttachment(ctx, req)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "update_manifest":
		var req manifestUpdateRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.UpdateManifest(ctx, req)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "rename_workspace":
		var req renameWorkspaceRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.RenameWorkspace(ctx, req)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "close_panes":
		var req closePanesRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.ClosePanes(ctx, req)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "kill_workspace":
		var req killWorkspaceRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		killAPI := api
		killAPI.client.Timeout = 15 * time.Second
		result, err := killAPI.KillWorkspace(ctx, req.WorkspaceID)
		value = result
		if err != nil {
			return nil, err
		}
	case "commit_move":
		var req moveCommitRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		result, err := api.CommitMove(ctx, req)
		value = result
		if err != nil {
			return nil, err
		}
	case "detach_attachment":
		var req attachmentRefRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.DetachAttachment(ctx, req.Workspace, req.Attachment)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "seen":
		var req workspacePaneRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.Seen(ctx, req.Workspace, req.Pane)
		value = workspace
		if err != nil {
			return nil, err
		}
	case "pane_readiness":
		var req paneReadinessRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := requireAuthoritative(ctx, api, req.Workspace); err != nil {
			return nil, err
		}
		workspace, err := api.Workspace(ctx, req.Workspace)
		if err != nil {
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
		value = paneReadinessResponse{
			BackendReady: pane.BackendReady,
			BackendDead:  pane.BackendDead,
			ClientReady:  clientHeartbeatFresh(attachment.ClientHeartbeats[req.Pane], time.Now().UTC()),
		}
	default:
		return nil, fmt.Errorf("unknown remote operation %q", op)
	}
	return json.Marshal(value)
}

func clientHeartbeatFresh(heartbeat, now time.Time) bool {
	age := now.Sub(heartbeat)
	return !heartbeat.IsZero() && age >= -time.Second && age <= 6*time.Second
}

func authoritativeWorkspaces(ctx context.Context, api API) ([]*Workspace, error) {
	workspaces, err := api.Workspaces(ctx)
	if err != nil {
		return nil, err
	}
	result := workspaces[:0]
	for _, workspace := range workspaces {
		if workspace.RemoteHost == "" {
			result = append(result, workspace)
		}
	}
	return result, nil
}

func requireAuthoritative(ctx context.Context, api API, ref string) error {
	workspace, err := api.Workspace(ctx, ref)
	if err != nil {
		return err
	}
	if workspace.RemoteHost != "" {
		return fmt.Errorf("workspace %q is not authoritative on this host", ref)
	}
	return nil
}

func (d *Daemon) cacheRemoteWorkspace(host string, remote *Workspace) *Workspace {
	if remote == nil || remote.ID == "" {
		return nil
	}
	d.mu.Lock()
	clone := remote.Clone()
	clone.RemoteHost = host
	normalizeWorkspace(clone)
	var changedPanes []string
	var pendingDetaches []string
	var revokedEndpoints []string
	var deletingEndpoints []string
	existing := d.state.Workspaces[clone.ID]
	if existing != nil {
		for paneID, pane := range clone.Panes {
			if previous := existing.Panes[paneID]; previous != nil {
				if pane.Notifications == nil {
					pane.Notifications = map[string]NotificationRecord{}
				}
				for key, record := range previous.Notifications {
					if _, present := pane.Notifications[key]; !present {
						pane.Notifications[key] = record
					}
				}
				if previous.State != pane.State {
					changedPanes = append(changedPanes, paneID)
				}
			}
		}
		for id, localAttachment := range existing.Attachments {
			if !strings.HasPrefix(localAttachment.Endpoint, "unix:") {
				continue
			}
			if authoritative := clone.Attachments[id]; authoritative != nil {
				local := localAttachment.Clone()
				local.Role = authoritative.Role
				local.Revoked = authoritative.Revoked
				local.ClientHeartbeats = authoritative.ClientHeartbeats
				for paneID := range local.Views {
					pane := clone.Panes[paneID]
					if pane == nil || pane.RemovalPending {
						delete(local.Views, paneID)
					}
				}
				if authoritative.Status == AttachmentDetached && local.Status != AttachmentDetached {
					local.Revoked = true
				}
				if !local.Revoked {
					local.RevocationClosed = false
				}
				if local.Revoked && !local.RevocationClosed && local.Status != AttachmentDetached {
					revokedEndpoints = append(revokedEndpoints, local.Endpoint)
				}
				if authoritative.AppliedRevision == clone.Revision {
					local.AppliedRevision = clone.Revision
				}
				if local.Status == AttachmentDetached && authoritative.Status != AttachmentDetached {
					pendingDetaches = append(pendingDetaches, id)
				}
				clone.Attachments[id] = local
			} else {
				clone.Attachments[id] = localAttachment.Clone()
			}
			if clone.DeletionPending && localAttachment.Status != AttachmentDetached {
				deletingEndpoints = append(deletingEndpoints, localAttachment.Endpoint)
			}
		}
	}
	d.state.Workspaces[clone.ID] = clone
	cache := d.state.Remotes[host]
	if cache == nil {
		cache = &RemoteCache{Host: host, Workspaces: map[string]*Workspace{}}
		d.state.Remotes[host] = cache
	}
	cache.Workspaces[clone.ID] = remote.Clone()
	cache.UpdatedAt = time.Now().UTC()
	cache.LastError = ""
	_ = d.store.Save(d.state)
	result := clone.Clone()
	d.mu.Unlock()
	if existing != nil {
		if len(changedPanes) == 0 {
			d.startWorker(func(ctx context.Context) { d.updateKittyState(ctx, result) })
		}
		for _, paneID := range changedPanes {
			paneID := paneID
			d.startWorker(func(ctx context.Context) { d.afterRemoteTransition(ctx, result, paneID) })
		}
		for _, attachmentID := range pendingDetaches {
			attachmentID := attachmentID
			d.startWorker(func(ctx context.Context) {
				remoteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, _ = d.remotes.Call(remoteCtx, host, "detach_attachment", attachmentRefRequest{Workspace: result.ID, Attachment: attachmentID})
				cancel()
			})
		}
		for _, endpoint := range revokedEndpoints {
			d.scheduleCapture(endpoint)
		}
		for _, endpoint := range deletingEndpoints {
			endpoint := endpoint
			d.startWorker(func(ctx context.Context) {
				callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = d.kitty.CloseWorkspace(callCtx, endpoint, result.ID)
				cancel()
			})
		}
	}
	return result
}

func (d *Daemon) cacheRemoteSnapshot(host string, workspaces []*Workspace) {
	present := map[string]bool{}
	for _, workspace := range workspaces {
		if workspace == nil || workspace.ID == "" {
			continue
		}
		present[workspace.ID] = true
		d.cacheRemoteWorkspace(host, workspace)
	}
	d.mu.Lock()
	staleSet := map[string]bool{}
	if cache := d.state.Remotes[host]; cache != nil {
		for id := range cache.Workspaces {
			if !present[id] {
				staleSet[id] = true
			}
		}
	}
	for id, workspace := range d.state.Workspaces {
		if workspace.RemoteHost == host && !present[id] {
			staleSet[id] = true
		}
	}
	d.mu.Unlock()
	for id := range staleSet {
		d.evictRemoteWorkspace(host, id)
	}
}

func (d *Daemon) evictRemoteWorkspace(host, workspaceID string) {
	if workspaceID == "" {
		return
	}
	d.mu.Lock()
	var endpoints []string
	workspace := d.state.Workspaces[workspaceID]
	if workspace != nil && workspace.RemoteHost == host {
		for _, attachment := range workspace.Attachments {
			if attachment.Node.ID == d.state.Node.ID && attachment.Status != AttachmentDetached && strings.HasPrefix(attachment.Endpoint, "unix:") {
				attachment.Status = AttachmentDetached
				attachment.Views = map[string]RuntimeView{}
				endpoints = append(endpoints, attachment.Endpoint)
			}
		}
		delete(d.state.Workspaces, workspaceID)
	}
	if cache := d.state.Remotes[host]; cache != nil {
		delete(cache.Workspaces, workspaceID)
		cache.UpdatedAt = time.Now().UTC()
	}
	_ = d.store.Save(d.state)
	d.mu.Unlock()
	_ = d.store.RemoveWorkspaceSessions(workspaceID)
	for _, endpoint := range endpoints {
		endpoint := endpoint
		d.startWorker(func(ctx context.Context) {
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = d.kitty.CloseWorkspace(callCtx, endpoint, workspaceID)
			cancel()
		})
	}
}
