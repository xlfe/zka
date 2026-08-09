package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

const cardLeaseProtocolVersion = 1

type smartCardLease struct{ token chan struct{} }

func newSmartCardLease() *smartCardLease {
	lease := &smartCardLease{token: make(chan struct{}, 1)}
	lease.token <- struct{}{}
	return lease
}

func (l *smartCardLease) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.token:
		var once sync.Once
		return func() {
			once.Do(func() {
				l.token <- struct{}{}
			})
		}, nil
	}
}

type cardLeaseRequest struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
}

type cardLeaseResponse struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

func (d *Daemon) cardLeaseLoop(ctx context.Context) {
	for {
		conn, err := d.cardLeaseListener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.failWorker(fmt.Errorf("accept smart-card lease connection: %w", err))
			}
			return
		}
		client := conn
		d.startWorker(func(workerCtx context.Context) { d.handleCardLease(workerCtx, client) })
	}
}

func (d *Daemon) handleCardLease(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if !sameUIDUnixPeer(conn, uint32(os.Getuid())) {
		_ = json.NewEncoder(conn).Encode(cardLeaseResponse{Version: cardLeaseProtocolVersion, Error: "socket peer UID does not match zkad"})
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var req cardLeaseRequest
	dec := json.NewDecoder(io.LimitReader(conn, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Version != cardLeaseProtocolVersion || req.Operation == "" {
		_ = json.NewEncoder(conn).Encode(cardLeaseResponse{Version: cardLeaseProtocolVersion, Error: "invalid smart-card lease request"})
		return
	}
	_ = conn.SetDeadline(time.Time{})
	release, err := d.cardLease.acquire(ctx)
	if err != nil {
		return
	}
	defer release()
	if err := json.NewEncoder(conn).Encode(cardLeaseResponse{Version: cardLeaseProtocolVersion, OK: true}); err != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_, _ = bufio.NewReader(conn).ReadByte()
}

func sameUIDUnixPeer(conn net.Conn, want uint32) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return false
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || socketErr != nil || credential == nil {
		return false
	}
	return credential.Uid == want
}
