package zka

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

type remoteClientHello struct {
	Node Host `json:"node"`
}

type remoteServerHello struct {
	Node Host `json:"node"`
}

// credentialTargetSession publishes one private origin-side broker for an
// authenticated provider control session. Stable workspace listeners connect
// to this broker; each connection becomes a yamux stream back to the provider.
// Replacing the SSH session therefore changes no pane-facing path.
type credentialTargetSession struct {
	ctx        context.Context
	cancel     context.CancelFunc
	api        API
	provider   Host
	session    *yamux.Session
	endpoint   string
	broker     net.Listener
	brokerInfo os.FileInfo

	mu          sync.Mutex
	brokerConns map[net.Conn]net.Conn
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

// credentialProviderReconnectLoop restores provider control transports after a
// daemon restart even when the workspace has no local Kitty attachment. The
// durable binding is node-owned, so its liveness cannot depend on view state or
// on the user issuing another remote command.
func (d *Daemon) credentialProviderReconnectLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		d.reconnectCredentialProviders(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) reconnectCredentialProviders(ctx context.Context) {
	for host, workspaceID := range d.credentialProviderReconnectTargets() {
		status := d.remotes.credentialTransportStatusForHost(host)
		if status.State != "idle" {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = d.remotes.Call(callCtx, host, "get", refRequest{Ref: workspaceID})
		cancel()
	}
}

func (d *Daemon) credentialProviderReconnectTargets() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	targets := map[string]string{}
	providerNodeID := d.state.Node.ID
	add := func(host string, workspace *Workspace) {
		if host == "" || workspace == nil || workspace.CredentialClaim == nil ||
			workspace.CredentialClaim.ProviderSource == "local" || workspace.CredentialClaim.OwnerNodeID != providerNodeID {
			return
		}
		if targets[host] == "" {
			targets[host] = workspace.ID
		}
	}
	for _, workspace := range d.state.Workspaces {
		add(workspace.RemoteHost, workspace)
	}
	for host, remote := range d.state.Remotes {
		if remote == nil {
			continue
		}
		for _, workspace := range remote.Workspaces {
			add(host, workspace)
		}
	}
	return targets
}

func newCredentialTargetSession(parent context.Context, paths Paths, session *yamux.Session, provider Host) (*credentialTargetSession, error) {
	if provider.ID == "" {
		return nil, fmt.Errorf("credential transport provider has no node id")
	}
	ctx, cancel := context.WithCancel(parent)
	token, err := randomID()
	if err != nil {
		cancel()
		return nil, err
	}
	endpoint := filepath.Join(paths.RuntimeDir, "credential-transports", shortID(provider.ID)+"-"+shortID(token)+".sock")
	broker, err := listenUnix(endpoint)
	if err != nil {
		cancel()
		return nil, err
	}
	brokerInfo, err := os.Lstat(endpoint)
	if err != nil {
		_ = broker.Close()
		cancel()
		return nil, err
	}
	target := &credentialTargetSession{
		ctx: ctx, cancel: cancel, api: NewAPI(paths), provider: provider, session: session,
		endpoint: endpoint, broker: broker, brokerInfo: brokerInfo, brokerConns: map[net.Conn]net.Conn{},
	}
	registerCtx, registerCancel := context.WithTimeout(ctx, 750*time.Millisecond)
	registerErr := target.api.setCredentialTransport(registerCtx, credentialTransportSessionRequest{Provider: provider, State: "ready", Endpoint: endpoint})
	registerCancel()
	if registerErr != nil {
		_ = broker.Close()
		cancel()
		return nil, registerErr
	}
	target.wg.Add(2)
	go func() {
		defer target.wg.Done()
		target.serveBroker()
	}()
	go func() {
		defer target.wg.Done()
		target.heartbeatLoop()
	}()
	return target, nil
}

func (s *credentialTargetSession) close() {
	s.closeOnce.Do(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.api.setCredentialTransport(closeCtx, credentialTransportSessionRequest{Provider: s.provider, State: "disconnected", Endpoint: s.endpoint})
		closeCancel()
		s.cancel()
		_ = s.broker.Close()
		s.mu.Lock()
		brokerConns := make(map[net.Conn]net.Conn, len(s.brokerConns))
		for conn, stream := range s.brokerConns {
			brokerConns[conn] = stream
		}
		s.brokerConns = map[net.Conn]net.Conn{}
		s.mu.Unlock()
		for conn, stream := range brokerConns {
			_ = conn.Close()
			_ = stream.Close()
		}
		s.wg.Wait()
		if current, err := os.Lstat(s.endpoint); err == nil && os.SameFile(s.brokerInfo, current) {
			_ = os.Remove(s.endpoint)
		}
	})
}

func (s *credentialTargetSession) heartbeatLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(s.ctx, 750*time.Millisecond)
		_ = s.api.setCredentialTransport(ctx, credentialTransportSessionRequest{Provider: s.provider, State: "ready", Endpoint: s.endpoint})
		cancel()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *credentialTargetSession) serveBroker() {
	for {
		conn, err := s.broker.Accept()
		if err != nil {
			return
		}
		stream, err := s.session.OpenStream()
		if err != nil {
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		s.brokerConns[conn] = stream
		s.mu.Unlock()
		go func() {
			defer func() {
				_ = conn.Close()
				_ = stream.Close()
				s.mu.Lock()
				delete(s.brokerConns, conn)
				s.mu.Unlock()
			}()
			proxyRawCredentialTransport(s.ctx, conn, stream)
		}()
	}
}
