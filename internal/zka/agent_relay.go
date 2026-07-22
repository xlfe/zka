package zka

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	agentRelayVersion     = 1
	agentRelayDialTimeout = 500 * time.Millisecond
)

type agentSource struct {
	Attachment string
	Pane       string
	Socket     string
	Heartbeat  time.Time
}

func (s agentSource) key() string { return s.Attachment + "\x00" + s.Pane }

type agentProxy struct {
	client   net.Conn
	upstream net.Conn
}

type agentRelay struct {
	mu      sync.Mutex
	proxyWG sync.WaitGroup

	workspace    string
	path         string
	originSocket string
	listener     net.Listener
	boundInfo    os.FileInfo
	claimed      string
	selected     string
	generation   uint64
	sources      map[string]agentSource
	active       map[*agentProxy]struct{}
	done         chan struct{}
	closed       bool
}

type agentRelayManager struct {
	mu sync.Mutex
	wg sync.WaitGroup

	dir          string
	originSocket string
	relays       map[string]*agentRelay
	closed       bool
}

func newAgentRelayManager(dir, originSocket string) *agentRelayManager {
	return &agentRelayManager{
		dir:          dir,
		originSocket: originSocket,
		relays:       map[string]*agentRelay{},
	}
}

func (m *agentRelayManager) ensure(workspaceID, claimedAttachment string) (string, error) {
	if workspaceID == "" || filepath.Base(workspaceID) != workspaceID || strings.ContainsRune(workspaceID, os.PathSeparator) {
		return "", fmt.Errorf("invalid workspace id for agent relay")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", net.ErrClosed
	}
	if relay := m.relays[workspaceID]; relay != nil {
		relay.setClaim(claimedAttachment)
		return relay.path, nil
	}
	path := agentRelaySocketPath(m.dir, workspaceID)
	listener, err := listenUnix(path)
	if err != nil {
		return "", fmt.Errorf("start SSH agent relay for workspace %s: %w", shortID(workspaceID), err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("inspect SSH agent relay for workspace %s: %w", shortID(workspaceID), err)
	}
	relay := &agentRelay{
		workspace: workspaceID, path: path, originSocket: m.originSocket,
		listener: listener, boundInfo: info, claimed: claimedAttachment,
		sources: map[string]agentSource{}, active: map[*agentProxy]struct{}{}, done: make(chan struct{}),
	}
	m.relays[workspaceID] = relay
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		relay.serve()
	}()
	return path, nil
}

func (m *agentRelayManager) path(workspaceID string) string {
	return agentRelaySocketPath(m.dir, workspaceID)
}

// Linux limits Unix socket addresses to 107 path bytes. Keep ordinary runtime
// paths readable, but use a deterministic digest when a long test or custom
// runtime directory would otherwise make the relay impossible to bind.
func agentRelaySocketPath(dir, workspaceID string) string {
	const safeUnixSocketPath = 103
	path := filepath.Join(dir, workspaceID+".sock")
	if len(path) <= safeUnixSocketPath {
		return path
	}
	digest := sha256.Sum256([]byte(workspaceID))
	encoded := hex.EncodeToString(digest[:])
	available := safeUnixSocketPath - len(dir) - len(string(os.PathSeparator)) - len(".sock")
	if available < 8 {
		return path
	}
	if available > len(encoded) {
		available = len(encoded)
	}
	return filepath.Join(dir, encoded[:available]+".sock")
}

func (m *agentRelayManager) setClaim(workspaceID, attachmentID string) {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	if relay != nil {
		relay.setClaim(attachmentID)
	}
}

func (m *agentRelayManager) register(workspaceID string, source agentSource, ready bool) {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	if relay != nil {
		relay.register(source, ready)
	}
}

func (m *agentRelayManager) clearAttachment(workspaceID, attachmentID string) {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	if relay != nil {
		relay.clearAttachment(attachmentID)
	}
}

func (m *agentRelayManager) clearPane(workspaceID, paneID string) {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	if relay != nil {
		relay.clearPane(paneID)
	}
}

func (m *agentRelayManager) available(workspaceID, attachmentID string) bool {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	return relay != nil && relay.available(attachmentID)
}

func (m *agentRelayManager) sourceAvailable(workspaceID, attachmentID string) bool {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	m.mu.Unlock()
	return relay != nil && relay.sourceAvailable(attachmentID)
}

func (m *agentRelayManager) remove(workspaceID string) {
	m.mu.Lock()
	relay := m.relays[workspaceID]
	delete(m.relays, workspaceID)
	m.mu.Unlock()
	if relay != nil {
		relay.close()
		relay.proxyWG.Wait()
	}
}

func (m *agentRelayManager) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	relays := make([]*agentRelay, 0, len(m.relays))
	for _, relay := range m.relays {
		relays = append(relays, relay)
	}
	m.relays = map[string]*agentRelay{}
	m.mu.Unlock()
	for _, relay := range relays {
		relay.close()
	}
	m.wg.Wait()
	for _, relay := range relays {
		relay.proxyWG.Wait()
	}
}

func (r *agentRelay) serve() {
	reaperDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer close(reaperDone)
		for {
			select {
			case <-r.done:
				return
			case now := <-ticker.C:
				r.mu.Lock()
				if r.closed {
					r.mu.Unlock()
					return
				}
				r.expireLocked(now.UTC())
				r.mu.Unlock()
			}
		}
	}()
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				r.close()
			}
			<-reaperDone
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = conn.Close()
			continue
		}
		r.proxyWG.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.proxyWG.Done()
			r.proxy(conn)
		}()
	}
}

func (r *agentRelay) proxy(client net.Conn) {
	var proxy *agentProxy
	for attempts := 0; attempts < 3; attempts++ {
		upstream, generation, err := r.dialSelected("")
		if err != nil {
			_ = client.Close()
			return
		}
		proxy = &agentProxy{client: client, upstream: upstream}
		r.mu.Lock()
		if !r.closed && generation == r.generation {
			r.active[proxy] = struct{}{}
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
		_ = upstream.Close()
		proxy = nil
	}
	if proxy == nil {
		_ = client.Close()
		return
	}

	done := make(chan struct{}, 2)
	copyStream := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyStream(proxy.upstream, client)
	go copyStream(client, proxy.upstream)
	<-done
	_ = client.Close()
	_ = proxy.upstream.Close()
	<-done
	r.mu.Lock()
	delete(r.active, proxy)
	r.mu.Unlock()
}

func (r *agentRelay) dialSelected(attachmentOverride string) (net.Conn, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, r.generation, net.ErrClosed
	}
	now := time.Now().UTC()
	r.expireLocked(now)
	attachment := r.claimed
	if attachmentOverride != "" {
		attachment = attachmentOverride
	}
	if attachment == "" {
		conn, err := dialAgentSocket(r.originSocket)
		return conn, r.generation, err
	}
	if r.selected != "" {
		if source, ok := r.sources[r.selected]; ok && source.Attachment == attachment && clientHeartbeatFresh(source.Heartbeat, now) {
			conn, err := dialAgentSocket(source.Socket)
			if err == nil {
				return conn, r.generation, nil
			}
			delete(r.sources, r.selected)
		}
		r.selected = ""
		r.closeActiveLocked()
	}
	candidates := make([]agentSource, 0)
	for _, source := range r.sources {
		if source.Attachment == attachment && clientHeartbeatFresh(source.Heartbeat, now) {
			candidates = append(candidates, source)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Heartbeat.Equal(candidates[j].Heartbeat) {
			return candidates[i].Pane < candidates[j].Pane
		}
		return candidates[i].Heartbeat.After(candidates[j].Heartbeat)
	})
	var lastErr error
	for _, source := range candidates {
		conn, err := dialAgentSocket(source.Socket)
		if err != nil {
			lastErr = err
			continue
		}
		if r.selected != source.key() {
			r.closeActiveLocked()
			r.selected = source.key()
		}
		return conn, r.generation, nil
	}
	if lastErr != nil {
		return nil, r.generation, lastErr
	}
	return nil, r.generation, fmt.Errorf("no fresh forwarded SSH agent")
}

func dialAgentSocket(path string) (net.Conn, error) {
	if path == "" || path == "none" {
		return nil, fmt.Errorf("SSH agent is unavailable")
	}
	return net.DialTimeout("unix", path, agentRelayDialTimeout)
}

func (r *agentRelay) available(attachmentID string) bool {
	conn, _, err := r.dialSelected(attachmentID)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (r *agentRelay) sourceAvailable(attachmentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || attachmentID == "" {
		return false
	}
	now := time.Now().UTC()
	r.expireLocked(now)
	candidates := make([]agentSource, 0)
	for _, source := range r.sources {
		if source.Attachment == attachmentID && clientHeartbeatFresh(source.Heartbeat, now) {
			candidates = append(candidates, source)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Heartbeat.Equal(candidates[j].Heartbeat) {
			return candidates[i].Pane < candidates[j].Pane
		}
		return candidates[i].Heartbeat.After(candidates[j].Heartbeat)
	})
	for _, source := range candidates {
		conn, err := dialAgentSocket(source.Socket)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func (r *agentRelay) setClaim(attachmentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed == attachmentID {
		return
	}
	r.claimed = attachmentID
	r.selected = ""
	r.closeActiveLocked()
}

func (r *agentRelay) register(source agentSource, ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := source.key()
	if !ready || source.Socket == "" {
		delete(r.sources, key)
		if r.selected == key {
			r.selected = ""
			r.closeActiveLocked()
		}
		return
	}
	if previous, ok := r.sources[key]; ok && r.selected == key && previous.Socket != source.Socket {
		r.closeActiveLocked()
	}
	r.sources[key] = source
}

func (r *agentRelay) clearAttachment(attachmentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, source := range r.sources {
		if source.Attachment == attachmentID {
			delete(r.sources, key)
		}
	}
	if selected, ok := r.sources[r.selected]; r.selected != "" && (!ok || selected.Attachment == attachmentID) {
		r.selected = ""
		r.closeActiveLocked()
	}
}

func (r *agentRelay) clearPane(paneID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	selectedRemoved := false
	for key, source := range r.sources {
		if source.Pane == paneID {
			delete(r.sources, key)
			selectedRemoved = selectedRemoved || key == r.selected
		}
	}
	if selectedRemoved {
		r.selected = ""
		r.closeActiveLocked()
	}
}

func (r *agentRelay) expireLocked(now time.Time) {
	selectedExpired := false
	for key, source := range r.sources {
		if clientHeartbeatFresh(source.Heartbeat, now) {
			continue
		}
		delete(r.sources, key)
		selectedExpired = selectedExpired || key == r.selected
	}
	if selectedExpired {
		r.selected = ""
		r.closeActiveLocked()
	}
}

func (r *agentRelay) closeActiveLocked() {
	r.generation++
	for proxy := range r.active {
		_ = proxy.client.Close()
		_ = proxy.upstream.Close()
	}
}

func (r *agentRelay) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	_ = r.listener.Close()
	r.closeActiveLocked()
	boundInfo := r.boundInfo
	path := r.path
	r.mu.Unlock()
	if current, err := os.Lstat(path); err == nil && os.SameFile(boundInfo, current) {
		_ = os.Remove(path)
	}
}
