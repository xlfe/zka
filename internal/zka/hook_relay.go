package zka

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	hookRelayProtocolVersion = 1
	hookRelayMaxMessage      = 4 << 10
	hookRelayMaxTurnID       = 128
	hookRelayMaxDetail       = 180

	hookRelayRawRate          = 100.0
	hookRelayRawBurst         = 100.0
	hookRelayNoncriticalRate  = 10.0
	hookRelayNoncriticalBurst = 10.0
	hookRelayCriticalRate     = 10.0
	hookRelayCriticalBurst    = 10.0
	hookRelayMaxParsers       = 8
	hookRelayReorderWindow    = 128
	hookRelayQueueSize        = 8

	hookRelayClientTimeout   = 100 * time.Millisecond
	hookRelayReadTimeout     = 100 * time.Millisecond
	hookRelayUpstreamTimeout = 500 * time.Millisecond
	hookRelayShutdownGrace   = 2 * time.Second
	hookRelayChildStopGrace  = 5 * time.Second
	hookRelayLogInterval     = time.Minute
	hookRelayAcceptRetryBase = 5 * time.Millisecond
	hookRelayAcceptRetryMax  = time.Second
	hookRelayLockRetryDelay  = 10 * time.Millisecond

	// This grace is part of the flock registration protocol, not a cleanup
	// tuning knob. It prevents a concurrent startup sweep from observing a new
	// session directory before its creator has opened and locked session.lock.
	hookRelayStaleGrace = 60 * time.Second
)

const hookRelaySocketEnvironment = "ZKA_HOOK_RELAY_SOCKET"

type hookRelayRequest struct {
	Version int    `json:"version"`
	Agent   string `json:"agent"`
	Kind    string `json:"kind"`
	TurnID  string `json:"turn_id,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type hookRelaySocketRecord struct {
	Version int    `json:"version"`
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
}

type hookRelayParseResult struct {
	sequence uint64
	request  *hookRelayRequest
}

type hookRelayStats struct {
	rawDropped         atomic.Uint64
	invalid            atomic.Uint64
	noncriticalDropped atomic.Uint64
	criticalDropped    atomic.Uint64
	evicted            atomic.Uint64
}

type hookRelayClock func() time.Time

type hookRelaySignalAction uint8

const (
	hookRelaySignalIgnore hookRelaySignalAction = iota
	hookRelaySignalForward
	hookRelaySignalKill
)

type hookRelaySignalPolicy map[os.Signal]int

func (p hookRelaySignalPolicy) Action(sig os.Signal) hookRelaySignalAction {
	switch sig {
	case os.Interrupt, syscall.SIGQUIT:
		return hookRelaySignalIgnore
	case syscall.SIGHUP, syscall.SIGTERM:
		p[sig]++
		if p[sig] > 1 {
			return hookRelaySignalKill
		}
		return hookRelaySignalForward
	default:
		return hookRelaySignalIgnore
	}
}

type hookRelayBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    hookRelayClock
}

func newHookRelayBucket(rate, burst float64, now hookRelayClock) *hookRelayBucket {
	if now == nil {
		now = time.Now
	}
	current := now()
	return &hookRelayBucket{rate: rate, burst: burst, tokens: burst, last: current, now: now}
}

func (b *hookRelayBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type hookRelayUpstream interface {
	Event(context.Context, Event) (*Workspace, error)
}

type hookRelayListener interface {
	Accept() (net.Conn, error)
	Close() error
}

type hookRelayScheduler struct {
	mu        sync.Mutex
	queue     []Event
	wake      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	pending   sync.WaitGroup
	upstream  hookRelayUpstream
	logger    *log.Logger
	stats     *hookRelayStats
	unhealthy bool
	retired   bool
}

func newHookRelayScheduler(upstream hookRelayUpstream, logger *log.Logger, stats *hookRelayStats) *hookRelayScheduler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &hookRelayScheduler{
		wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		upstream: upstream, logger: logger, stats: stats,
	}
	go s.loop()
	return s
}

func hookRelayCritical(kind string) bool {
	switch kind {
	case "permission_request", "stop", "agent_error":
		return true
	default:
		return false
	}
}

func (s *hookRelayScheduler) Submit(event Event) {
	s.mu.Lock()
	if len(s.queue) == hookRelayQueueSize {
		drop := -1
		for i := range s.queue {
			if !hookRelayCritical(s.queue[i].Kind) {
				drop = i
				break
			}
		}
		if drop < 0 {
			drop = 0
		}
		copy(s.queue[drop:], s.queue[drop+1:])
		s.queue = s.queue[:len(s.queue)-1]
		s.pending.Done()
		s.stats.evicted.Add(1)
	}
	s.queue = append(s.queue, event)
	s.pending.Add(1)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *hookRelayScheduler) pop() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Event{}, false
	}
	event := s.queue[0]
	copy(s.queue, s.queue[1:])
	s.queue = s.queue[:len(s.queue)-1]
	return event, true
}

func (s *hookRelayScheduler) loop() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			s.discardQueued()
			return
		case <-s.wake:
		}
		for {
			event, ok := s.pop()
			if !ok {
				break
			}
			ctx, cancel := context.WithTimeout(s.ctx, hookRelayUpstreamTimeout)
			_, err := s.upstream.Event(ctx, event)
			cancel()
			s.recordResult(err)
			s.pending.Done()
		}
	}
}

func (s *hookRelayScheduler) discardQueued() {
	s.mu.Lock()
	queued := len(s.queue)
	s.queue = nil
	s.mu.Unlock()
	for range queued {
		s.pending.Done()
	}
}

func (s *hookRelayScheduler) recordResult(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		if s.unhealthy || s.retired {
			s.logger.Printf("upstream delivery recovered")
		}
		s.unhealthy, s.retired = false, false
		return
	}
	if strings.HasPrefix(err.Error(), "unknown pane ") {
		if !s.retired {
			s.logger.Printf("bound pane retired; terminate or recreate this sandbox")
		}
		s.retired, s.unhealthy = true, false
		return
	}
	if !s.unhealthy {
		s.logger.Printf("upstream delivery unavailable: %v", err)
	}
	s.unhealthy, s.retired = true, false
}

func (s *hookRelayScheduler) Stop(grace time.Duration) {
	waited := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(grace):
		s.cancel()
		<-waited
	}
	s.cancel()
	<-s.done
}

type hookRelayServer struct {
	listener    hookRelayListener
	logger      *log.Logger
	stats       *hookRelayStats
	raw         *hookRelayBucket
	critical    *hookRelayBucket
	noncritical *hookRelayBucket
	scheduler   *hookRelayScheduler

	parserSlots chan struct{}
	window      chan struct{}
	results     chan hookRelayParseResult
	fatal       chan error
	acceptDone  chan struct{}
	orderedDone chan struct{}
	logStop     chan struct{}
	logDone     chan struct{}
	parsers     sync.WaitGroup
	sequence    atomic.Uint64
	stopOnce    sync.Once
	stopDone    chan struct{}
}

func newHookRelayServer(
	listener hookRelayListener,
	upstream hookRelayUpstream,
	workspaceID, paneID string,
	logger *log.Logger,
	now hookRelayClock,
) *hookRelayServer {
	stats := &hookRelayStats{}
	server := &hookRelayServer{
		listener: listener, logger: logger, stats: stats,
		raw:         newHookRelayBucket(hookRelayRawRate, hookRelayRawBurst, now),
		critical:    newHookRelayBucket(hookRelayCriticalRate, hookRelayCriticalBurst, now),
		noncritical: newHookRelayBucket(hookRelayNoncriticalRate, hookRelayNoncriticalBurst, now),
		parserSlots: make(chan struct{}, hookRelayMaxParsers),
		window:      make(chan struct{}, hookRelayReorderWindow),
		results:     make(chan hookRelayParseResult, hookRelayReorderWindow),
		fatal:       make(chan error, 1), acceptDone: make(chan struct{}), orderedDone: make(chan struct{}),
		logStop: make(chan struct{}), logDone: make(chan struct{}), stopDone: make(chan struct{}),
	}
	server.scheduler = newHookRelayScheduler(
		hookRelayAPI{upstream: upstream, workspaceID: workspaceID, paneID: paneID},
		logger,
		stats,
	)
	return server
}

type hookRelayAPI struct {
	upstream            hookRelayUpstream
	workspaceID, paneID string
}

func (a hookRelayAPI) Event(ctx context.Context, event Event) (*Workspace, error) {
	event.WorkspaceID = a.workspaceID
	event.PaneID = a.paneID
	return a.upstream.Event(ctx, event)
}

func (s *hookRelayServer) Start() {
	go s.acceptLoop()
	go s.orderedLoop()
	go s.logLoop()
}

func (s *hookRelayServer) acceptLoop() {
	defer close(s.acceptDone)
	var retryDelay time.Duration
	for {
		s.window <- struct{}{}
		conn, err := s.listener.Accept()
		if err != nil {
			<-s.window
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if hookRelayAcceptRetryable(err) {
				if retryDelay == 0 {
					retryDelay = hookRelayAcceptRetryBase
				} else {
					retryDelay = min(2*retryDelay, hookRelayAcceptRetryMax)
				}
				s.logger.Printf("temporary accept failure; retrying in %s: %v", retryDelay, err)
				time.Sleep(retryDelay)
				continue
			}
			select {
			case s.fatal <- fmt.Errorf("accept hook relay connection: %w", err):
			default:
			}
			return
		}
		retryDelay = 0
		sequence := s.sequence.Add(1)
		if !s.raw.Allow() {
			s.stats.rawDropped.Add(1)
			_ = conn.Close()
			s.results <- hookRelayParseResult{sequence: sequence}
			continue
		}
		select {
		case s.parserSlots <- struct{}{}:
			s.parsers.Add(1)
			go s.parse(sequence, conn)
		default:
			s.stats.rawDropped.Add(1)
			_ = conn.Close()
			s.results <- hookRelayParseResult{sequence: sequence}
		}
	}
}

func hookRelayAcceptRetryable(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ECONNABORTED)
}

func (s *hookRelayServer) parse(sequence uint64, conn net.Conn) {
	defer func() {
		_ = conn.Close()
		<-s.parserSlots
		s.parsers.Done()
	}()
	_ = conn.SetReadDeadline(time.Now().Add(hookRelayReadTimeout))
	payload, err := io.ReadAll(io.LimitReader(conn, hookRelayMaxMessage+1))
	if err != nil || len(payload) == 0 || len(payload) > hookRelayMaxMessage || !utf8.Valid(payload) {
		s.stats.invalid.Add(1)
		s.results <- hookRelayParseResult{sequence: sequence}
		return
	}
	var request hookRelayRequest
	if err := decodeStrictJSON(payload, &request); err != nil || !validateHookRelayRequest(&request) {
		s.stats.invalid.Add(1)
		s.results <- hookRelayParseResult{sequence: sequence}
		return
	}
	request.Detail = sanitizeHookRelayDetail(request.Detail)
	s.results <- hookRelayParseResult{sequence: sequence, request: &request}
}

func (s *hookRelayServer) orderedLoop() {
	defer close(s.orderedDone)
	next := uint64(1)
	waiting := make(map[uint64]hookRelayParseResult)
	for result := range s.results {
		waiting[result.sequence] = result
		for {
			current, ok := waiting[next]
			if !ok {
				break
			}
			delete(waiting, next)
			<-s.window
			if current.request != nil {
				bucket := s.noncritical
				dropped := &s.stats.noncriticalDropped
				if hookRelayCritical(current.request.Kind) {
					bucket = s.critical
					dropped = &s.stats.criticalDropped
				}
				if bucket.Allow() {
					s.scheduler.Submit(Event{
						Kind: current.request.Kind, Source: current.request.Agent + "-hook",
						TurnID: current.request.TurnID, Detail: current.request.Detail,
					})
				} else {
					dropped.Add(1)
				}
			}
			next++
		}
	}
}

func (s *hookRelayServer) logLoop() {
	defer close(s.logDone)
	ticker := time.NewTicker(hookRelayLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.logStop:
			s.flushStats()
			return
		case <-ticker.C:
			s.flushStats()
		}
	}
}

func (s *hookRelayServer) flushStats() {
	raw := s.stats.rawDropped.Swap(0)
	invalid := s.stats.invalid.Swap(0)
	noncritical := s.stats.noncriticalDropped.Swap(0)
	critical := s.stats.criticalDropped.Swap(0)
	evicted := s.stats.evicted.Swap(0)
	if raw+invalid+noncritical+critical+evicted != 0 {
		s.logger.Printf(
			"traffic summary: raw_dropped=%d invalid=%d noncritical_limited=%d critical_limited=%d queue_evicted=%d",
			raw, invalid, noncritical, critical, evicted,
		)
	}
}

func (s *hookRelayServer) Stop(grace time.Duration) {
	s.stopOnce.Do(func() {
		_ = s.listener.Close()
		<-s.acceptDone
		s.parsers.Wait()
		close(s.results)
		<-s.orderedDone
		s.scheduler.Stop(grace)
		close(s.logStop)
		<-s.logDone
		close(s.stopDone)
	})
	<-s.stopDone
}

func validateHookRelayRequest(request *hookRelayRequest) bool {
	if request.Version != hookRelayProtocolVersion ||
		(request.Agent != "codex" && request.Agent != "claude") ||
		!hookRelayKindAllowed(request.Kind) {
		return false
	}
	if len(request.TurnID) > hookRelayMaxTurnID ||
		!utf8.ValidString(request.TurnID) ||
		containsHookRelayControl(request.TurnID) {
		return false
	}
	return true
}

func hookRelayKindAllowed(kind string) bool {
	switch kind {
	case "session_start", "user_prompt", "permission_request", "post_tool", "stop", "agent_error", "session_end":
		return true
	default:
		return false
	}
}

func containsHookRelayControl(value string) bool {
	for _, r := range value {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f {
			return true
		}
	}
	return false
}

func sanitizeHookRelayDetail(value string) string {
	value = strings.Map(func(r rune) rune {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= hookRelayMaxDetail {
		return value
	}
	return truncateUTF8(value, hookRelayMaxDetail)
}

func sendHookRelayEvent(socket string, request hookRelayRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), hookRelayClientTimeout)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: hookRelayClientTimeout}).DialContext(ctx, "unix", socket)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(hookRelayClientTimeout))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
}

type hookRelaySession struct {
	id        string
	dir       string
	socket    string
	lock      *os.File
	listener  *ownedUnixListener
	logger    *log.Logger
	closeOnce sync.Once
}

func hookRelayRoot(paths Paths) string { return filepath.Join(paths.RuntimeDir, "hook-relays") }

func prepareHookRelayRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create hook relay root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect hook relay root: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("hook relay root %q must be a real directory owned by the current user", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure hook relay root: %w", err)
	}
	return nil
}

func validHookRelaySessionID(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && value == strings.ToLower(value)
}

func createHookRelaySession(paths Paths, logger *log.Logger) (*hookRelaySession, error) {
	root := hookRelayRoot(paths)
	if err := prepareHookRelayRoot(root); err != nil {
		return nil, err
	}
	sweepHookRelaySessions(root, time.Now(), logger)
	var id, dir string
	for range 16 {
		var err error
		id, err = randomID()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(root, id)
		if err := os.Mkdir(dir, 0o700); err == nil {
			if err := os.Chmod(dir, 0o700); err != nil {
				_ = os.Remove(dir)
				return nil, fmt.Errorf("secure hook relay session: %w", err)
			}
			break
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create hook relay session: %w", err)
		}
		id = ""
	}
	if id == "" {
		return nil, errors.New("create hook relay session: too many identifier collisions")
	}
	session := &hookRelaySession{id: id, dir: dir, socket: agentRelaySocketPath(root, id), logger: logger}
	cleanup := true
	defer func() {
		if cleanup {
			session.Close()
		}
	}()
	if err := validateUnixSocketPath(session.socket); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create hook relay lock: %w", err)
	}
	session.lock = lock
	if err := lock.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure hook relay lock: %w", err)
	}
	if err := lockHookRelaySession(lock); err != nil {
		return nil, fmt.Errorf("lock hook relay session: %w", err)
	}
	listener, err := listenUnixExclusive(session.socket)
	if err != nil {
		return nil, err
	}
	session.listener = listener
	stat, ok := listener.boundInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("inspect hook relay socket inode")
	}
	record := hookRelaySocketRecord{Version: hookRelayProtocolVersion, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := writeExclusivePrivate(filepath.Join(dir, "socket.json"), append(encoded, '\n')); err != nil {
		return nil, fmt.Errorf("record hook relay socket inode: %w", err)
	}
	cleanup = false
	return session, nil
}

func lockHookRelaySession(lock *os.File) error {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		return err
	}
	time.Sleep(hookRelayLockRetryDelay)
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func writeExclusivePrivate(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	ok = true
	return file.Close()
}

func (s *hookRelaySession) Close() {
	s.closeOnce.Do(func() {
		if s.listener != nil {
			if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				s.logger.Printf("close hook relay listener: %v", err)
			}
		}
		if err := os.RemoveAll(s.dir); err != nil {
			s.logger.Printf("remove hook relay session %s: %v", s.id, err)
		}
		if s.lock != nil {
			_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
			_ = s.lock.Close()
		}
	})
}

func sweepHookRelaySessions(root string, now time.Time, logger *log.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		logger.Printf("inspect stale hook relays: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validHookRelaySessionID(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || now.Sub(info.ModTime()) < hookRelayStaleGrace {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			continue
		}
		socket := agentRelaySocketPath(root, entry.Name())
		owned, recordErr := hookRelayRecordedSocketOwned(dir, socket)
		if recordErr != nil {
			logger.Printf("inspect stale hook relay %s: %v", entry.Name(), recordErr)
		}
		if recordErr == nil && owned {
			if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Printf("remove stale hook relay socket %s: %v", entry.Name(), err)
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				_ = lock.Close()
				continue
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			logger.Printf("remove stale hook relay %s: %v", entry.Name(), err)
		}
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
}

func hookRelayRecordedSocketOwned(dir, socket string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "socket.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, socketErr := os.Lstat(socket); errors.Is(socketErr, os.ErrNotExist) {
				return false, nil
			}
		}
		return false, err
	}
	var record hookRelaySocketRecord
	if err := decodeStrictJSON(raw, &record); err != nil || record.Version != hookRelayProtocolVersion {
		return false, errors.New("invalid socket ownership record")
	}
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSocket == 0 || !ok ||
		uint64(stat.Dev) != record.Device || uint64(stat.Ino) != record.Inode {
		return false, errors.New("socket path no longer names the recorded inode")
	}
	return true, nil
}

func runRelay(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] != "hooks" {
		return 2, fmt.Errorf("relay supports only: zka relay hooks")
	}
	fs := newFlagSet("relay hooks", stderr)
	workspaceID := fs.String("workspace", os.Getenv("ZKA_WORKSPACE_ID"), "workspace id (defaults to ZKA_WORKSPACE_ID)")
	paneID := fs.String("pane", os.Getenv("ZKA_PANE_ID"), "pane id (defaults to ZKA_PANE_ID)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	command := fs.Args()
	if *workspaceID == "" || *paneID == "" || len(command) == 0 || command[0] == "" {
		return 2, errors.New("relay hooks requires workspace, pane, and a command after --")
	}
	return runHookRelaySupervisor(paths, *workspaceID, *paneID, command, stdin, stdout, stderr)
}

func runHookRelaySupervisor(
	paths Paths,
	workspaceID, paneID string,
	command []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) (int, error) {
	logger := log.New(stderr, "zka relay hooks: ", log.LstdFlags)
	session, err := createHookRelaySession(paths, logger)
	if err != nil {
		return 1, err
	}
	defer session.Close()
	server := newHookRelayServer(session.listener, NewAPI(paths), workspaceID, paneID, logger, nil)
	server.Start()
	logger.Printf("listening on %s", session.socket)

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = replaceEnvironmentValue(os.Environ(), hookRelaySocketEnvironment, session.socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(signals)
	started := make(chan error, 1)
	childDone := make(chan error, 1)
	go func() {
		// Linux keys Pdeathsig to the OS thread that calls fork, not merely to
		// this process. Pin that thread and keep this goroutine alive through
		// Wait; unlocking after Start can terminate an idle runtime thread and
		// send SIGTERM to a healthy long-running sandbox.
		runtime.LockOSThread()
		err := cmd.Start()
		started <- err
		if err == nil {
			childDone <- cmd.Wait()
		}
		runtime.UnlockOSThread()
	}()
	if err := <-started; err != nil {
		server.Stop(0)
		return 1, fmt.Errorf("start supervised sandbox launcher: %w", err)
	}

	signalPolicy := hookRelaySignalPolicy{}
	var terminationTimer *time.Timer
	var terminationDeadline <-chan time.Time
	defer func() {
		if terminationTimer != nil {
			terminationTimer.Stop()
		}
	}()
	for {
		select {
		case childErr := <-childDone:
			server.Stop(hookRelayShutdownGrace)
			logger.Printf("sandbox launcher exited")
			return processExitCode(childErr), nil
		case relayErr := <-server.fatal:
			childErr := terminateHookRelayChild(cmd, childDone)
			server.Stop(0)
			if childErr != nil {
				logger.Printf("sandbox launcher stopped after relay failure: %v", childErr)
			}
			return 1, relayErr
		case <-terminationDeadline:
			terminationDeadline = nil
			_ = cmd.Process.Kill()
		case sig := <-signals:
			switch signalPolicy.Action(sig) {
			case hookRelaySignalIgnore:
				// The terminal delivers these to the whole foreground process
				// group. Forwarding would turn the user's first signal into the
				// launcher's second and can force an otherwise graceful exit.
			case hookRelaySignalForward:
				if terminationTimer == nil {
					terminationTimer = time.NewTimer(hookRelayChildStopGrace)
					terminationDeadline = terminationTimer.C
				}
				_ = cmd.Process.Signal(sig)
			case hookRelaySignalKill:
				if terminationTimer == nil {
					terminationTimer = time.NewTimer(hookRelayChildStopGrace)
					terminationDeadline = terminationTimer.C
				}
				_ = cmd.Process.Kill()
			}
		}
	}
}

func terminateHookRelayChild(cmd *exec.Cmd, childDone <-chan error) error {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(hookRelayChildStopGrace)
	defer timer.Stop()
	select {
	case err := <-childDone:
		return err
	case <-timer.C:
		_ = cmd.Process.Kill()
		return <-childDone
	}
}
