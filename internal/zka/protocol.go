package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxProtocolMessage = 1 << 20
	safeUnixSocketPath = 103
)

// Leave enough time for zkad to serialize an operation deadline error before
// the client-side socket/context deadline closes the connection.
const daemonResponseGrace = 100 * time.Millisecond

type request struct {
	Version          int             `json:"version"`
	Op               string          `json:"op"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	DeadlineUnixNano int64           `json:"deadline_unix_nano,omitempty"`
}

type response struct {
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type Client struct {
	Socket  string
	Timeout time.Duration
}

func (c Client) Call(ctx context.Context, op string, payload, out any) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connect to zkad at %s: %w", c.Socket, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	daemonDeadline := deadline
	if time.Until(deadline) > 2*daemonResponseGrace {
		daemonDeadline = deadline.Add(-daemonResponseGrace)
	}
	var raw json.RawMessage
	if payload != nil {
		raw, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	if err := json.NewEncoder(conn).Encode(request{Version: daemonProtocolVersion, Op: op, Payload: raw, DeadlineUnixNano: daemonDeadline.UnixNano()}); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	var res response
	dec := json.NewDecoder(io.LimitReader(conn, maxProtocolMessage))
	if err := dec.Decode(&res); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read response: %w", err)
	}
	if res.Version != daemonProtocolVersion {
		return fmt.Errorf("unsupported daemon protocol %d (client requires %d; upgrade and restart zka on this machine)", res.Version, daemonProtocolVersion)
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	if out != nil && len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// WatchAttention owns a long-lived daemon connection and calls yield for the
// initial snapshot and each subsequent update. Reconnection policy belongs to
// the consumer so it can expose an unavailable state between daemon restarts.
func (c Client) WatchAttention(ctx context.Context, yield func(AttentionSnapshot) error) error {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connect to zkad at %s: %w", c.Socket, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(request{Version: daemonProtocolVersion, Op: "watch_attention"}); err != nil {
		return fmt.Errorf("send attention watch request: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	decoder := json.NewDecoder(conn)
	for {
		var res response
		if err := decoder.Decode(&res); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read attention update: %w", err)
		}
		if res.Version != daemonProtocolVersion {
			return fmt.Errorf("unsupported daemon protocol %d", res.Version)
		}
		if !res.OK {
			return errors.New(res.Error)
		}
		var snapshot AttentionSnapshot
		if err := json.Unmarshal(res.Data, &snapshot); err != nil {
			return fmt.Errorf("decode attention update: %w", err)
		}
		if err := yield(snapshot); err != nil {
			return err
		}
	}
}

type ownedUnixListener struct {
	*net.UnixListener
	path      string
	boundInfo os.FileInfo
	closeOnce sync.Once
	closeErr  error
}

func (l *ownedUnixListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.UnixListener.Close()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.boundInfo, current) {
			if err := os.Remove(l.path); l.closeErr == nil && err != nil && !errors.Is(err, os.ErrNotExist) {
				l.closeErr = err
			}
		}
	})
	return l.closeErr
}

func validateUnixSocketPath(path string) error {
	if len(path) > safeUnixSocketPath {
		return fmt.Errorf("Unix socket path is %d bytes; maximum is %d: shorten ZKA_RUNTIME_DIR", len(path), safeUnixSocketPath)
	}
	return nil
}

func bindUnix(path string, removeStale, prepareDirectory bool) (*ownedUnixListener, error) {
	if err := validateUnixSocketPath(path); err != nil {
		return nil, err
	}
	if prepareDirectory {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create runtime directory: %w", err)
		}
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("secure runtime directory: %w", err)
		}
	}
	if removeStale {
		if err := removeStaleSocket(path); err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("refusing to replace existing path %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("resolve Unix socket %s: %w", path, err)
	}
	ln, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	ln.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("inspect bound Unix socket: %w", err)
	}
	owned := &ownedUnixListener{UnixListener: ln, path: path, boundInfo: info}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = owned.Close()
		return nil, fmt.Errorf("secure Unix socket: %w", err)
	}
	return owned, nil
}

func listenUnix(path string) (net.Listener, error) {
	return bindUnix(path, true, true)
}

func listenUnixExclusive(path string) (*ownedUnixListener, error) {
	return bindUnix(path, false, false)
}

func listenUnixgram(path string) (*net.UnixConn, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secure runtime directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to remove non-socket path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale watcher socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect watcher socket: %w", err)
	}
	address := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("secure watcher socket: %w", err)
	}
	return conn, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect daemon socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("another zkad is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}
