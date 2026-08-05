package zka

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

type remoteClientHello struct {
	Node Host `json:"node"`
}

type remoteServerHello struct {
	Node Host `json:"node"`
}

// Credential socket topology (the SSH connection is initiated by provider):
//
//	provider laptop                                   target/origin devbox
//	IdentityAgent ── SSH byte proxy ──┐          ┌── stable SSH_AUTH_SOCK
//	gpg-agent extra socket            ├─ yamux ──┤
//	  └─ provider Assuan filter ──────┘          └── GNUPGHOME/S.gpg-agent
//
// The target owns only public keys and listener paths. Private-key policy,
// keygrip resolution, pinentry, touch, and notifications remain on provider.
type credentialTargetSession struct {
	ctx      context.Context
	cancel   context.CancelFunc
	paths    Paths
	api      API
	config   Config
	runner   CommandRunner
	provider Host
	session  *yamux.Session

	mu          sync.Mutex
	listeners   map[string]*credentialTargetListener
	socketPaths map[string]string
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

type credentialTargetListener struct {
	workspace  string
	bundle     string
	capability string
	generation uint64
	path       string
	listener   net.Listener
	boundInfo  os.FileInfo
	authorized atomic.Bool
	done       chan struct{}
	closeOnce  sync.Once
	mu         sync.Mutex
	active     map[net.Conn]struct{}
}

type desiredCredentialTarget struct {
	workspace  string
	bundle     string
	capability string
	generation uint64
	path       string
}

func newCredentialTargetSession(parent context.Context, paths Paths, session *yamux.Session, provider Host) (*credentialTargetSession, error) {
	if provider.ID == "" {
		return nil, fmt.Errorf("credential transport provider has no node id")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	target := &credentialTargetSession{
		ctx: ctx, cancel: cancel, paths: paths, api: NewAPI(paths), config: cfg,
		runner: ExecRunner{}, provider: provider, session: session,
		listeners: map[string]*credentialTargetListener{}, socketPaths: map[string]string{},
	}
	target.wg.Add(1)
	go func() {
		defer target.wg.Done()
		target.reconcileLoop()
	}()
	return target, nil
}

func (s *credentialTargetSession) close() {
	s.closeOnce.Do(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.api.setCredentialTransport(closeCtx, credentialTransportSessionRequest{Provider: s.provider, State: "disconnected"})
		closeCancel()
		s.cancel()
		s.mu.Lock()
		listeners := make([]*credentialTargetListener, 0, len(s.listeners))
		for _, listener := range s.listeners {
			listeners = append(listeners, listener)
		}
		s.listeners = map[string]*credentialTargetListener{}
		s.mu.Unlock()
		for _, listener := range listeners {
			listener.close()
		}
		s.wg.Wait()
	})
}

func (s *credentialTargetSession) reconcileLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.reconcile()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *credentialTargetSession) reconcile() {
	ctx, cancel := context.WithTimeout(s.ctx, 750*time.Millisecond)
	workspaces, err := s.api.credentialTargetSnapshot(ctx, s.provider)
	cancel()
	if err != nil {
		s.mu.Lock()
		for _, listener := range s.listeners {
			listener.authorized.Store(false)
		}
		s.mu.Unlock()
		return
	}
	desired := map[string]desiredCredentialTarget{}
	desiredSocketPaths := map[string]bool{}
	for _, workspace := range workspaces {
		if workspace.RemoteHost != "" || workspace.CredentialClaim == nil {
			continue
		}
		claim := workspace.CredentialClaim
		if claim.OwnerNodeID != s.provider.ID || claim.State == "unclaimed" {
			continue
		}
		bundle, ok := s.config.credentialBundle(claim.Bundle)
		if !ok {
			continue
		}
		if bundle.SSHAgent.Enable {
			target := desiredCredentialTarget{
				workspace: workspace.ID, bundle: claim.Bundle, capability: credentialCapabilitySSH,
				generation: claim.Generation, path: agentRelaySocketPath(s.paths.AgentDir, workspace.ID),
			}
			desired[target.capability+"\x00"+workspace.ID] = target
		}
		if bundle.OpenPGP.Enable {
			cacheKey := credentialTargetSocketCacheKey(workspace.ID, claim.Generation)
			desiredSocketPaths[cacheKey] = true
			pathCtx, pathCancel := context.WithTimeout(s.ctx, 750*time.Millisecond)
			path, pathErr := s.cachedOpenPGPSocketPath(pathCtx, workspace.ID, claim.Generation)
			pathCancel()
			if pathErr == nil {
				target := desiredCredentialTarget{
					workspace: workspace.ID, bundle: claim.Bundle, capability: credentialCapabilityOpenPGP,
					generation: claim.Generation, path: path,
				}
				desired[target.capability+"\x00"+workspace.ID] = target
			}
		}
	}

	s.mu.Lock()
	var closeListeners []*credentialTargetListener
	for key := range s.socketPaths {
		if !desiredSocketPaths[key] {
			delete(s.socketPaths, key)
		}
	}
	for key, listener := range s.listeners {
		target, ok := desired[key]
		if !ok || listener.generation != target.generation || listener.path != target.path || listener.bundle != target.bundle || !listener.socketPublished() {
			delete(s.listeners, key)
			closeListeners = append(closeListeners, listener)
			continue
		}
		listener.authorized.Store(true)
		delete(desired, key)
	}
	s.mu.Unlock()
	for _, listener := range closeListeners {
		listener.close()
	}
	for key, target := range desired {
		listener, err := s.startListener(target)
		if err != nil {
			continue
		}
		s.mu.Lock()
		if s.listeners[key] == nil {
			s.listeners[key] = listener
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		listener.close()
	}
}

func credentialTargetSocketCacheKey(workspaceID string, generation uint64) string {
	return workspaceID + "\x00" + fmt.Sprintf("%d", generation)
}

func (s *credentialTargetSession) cachedOpenPGPSocketPath(ctx context.Context, workspaceID string, generation uint64) (string, error) {
	cacheKey := credentialTargetSocketCacheKey(workspaceID, generation)
	s.mu.Lock()
	cached := s.socketPaths[cacheKey]
	s.mu.Unlock()
	if credentialSocketPublished(cached) {
		return cached, nil
	}
	path, err := s.resolveOpenPGPSocketPath(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.socketPaths[cacheKey] = path
	s.mu.Unlock()
	return path, nil
}

func credentialSocketPublished(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func (s *credentialTargetSession) resolveOpenPGPSocketPath(ctx context.Context, workspaceID string) (string, error) {
	home, err := credentialOpenPGPHome(s.paths, workspaceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	if err := ensureGPGSocketDirectory(ctx, s.config.Credentials.GnuPG.GPGConfCommand, home, s.runner); err != nil {
		return "", err
	}
	socket, _, err := s.runner.Run(ctx, s.config.Credentials.GnuPG.GPGConfCommand, "--homedir", home, "--list-dirs", "agent-socket")
	if err != nil {
		return "", err
	}
	socket = strings.TrimSpace(socket)
	if socket == "" || !filepath.IsAbs(socket) {
		return "", fmt.Errorf("gpgconf returned invalid agent socket %q", socket)
	}
	return socket, nil
}

func (l *credentialTargetListener) socketPublished() bool {
	current, err := os.Lstat(l.path)
	return err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(l.boundInfo, current)
}

func (s *credentialTargetSession) startListener(target desiredCredentialTarget) (*credentialTargetListener, error) {
	listener, err := listenUnix(target.path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(target.path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	targetListener := &credentialTargetListener{
		workspace: target.workspace, bundle: target.bundle, capability: target.capability,
		generation: target.generation, path: target.path, listener: listener, boundInfo: info,
		done: make(chan struct{}), active: map[net.Conn]struct{}{},
	}
	targetListener.authorized.Store(true)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		targetListener.serve(s.ctx, s.session)
	}()
	return targetListener, nil
}

func (l *credentialTargetListener) serve(ctx context.Context, session *yamux.Session) {
	for {
		client, err := l.listener.Accept()
		if err != nil {
			return
		}
		if !l.authorized.Load() {
			_ = client.Close()
			continue
		}
		stream, err := session.OpenStream()
		if err != nil {
			_ = client.Close()
			continue
		}
		hello := credentialStreamHello{
			Workspace: l.workspace, Bundle: l.bundle, Capability: l.capability, Generation: l.generation,
		}
		if err := writeCredentialStreamHello(stream, hello); err != nil {
			_ = stream.Close()
			_ = client.Close()
			continue
		}
		l.mu.Lock()
		l.active[client] = struct{}{}
		l.mu.Unlock()
		go l.proxy(ctx, client, stream)
	}
}

func (l *credentialTargetListener) proxy(ctx context.Context, client net.Conn, stream net.Conn) {
	defer func() {
		_ = client.Close()
		_ = stream.Close()
		l.mu.Lock()
		delete(l.active, client)
		l.mu.Unlock()
	}()
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyOne(stream, client)
	go copyOne(client, stream)
	select {
	case <-ctx.Done():
	case <-l.done:
	case <-done:
	}
}

func (l *credentialTargetListener) close() {
	l.closeOnce.Do(func() {
		l.authorized.Store(false)
		close(l.done)
		_ = l.listener.Close()
		l.mu.Lock()
		for conn := range l.active {
			_ = conn.Close()
		}
		l.mu.Unlock()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.boundInfo, current) {
			_ = os.Remove(l.path)
		}
	})
}
