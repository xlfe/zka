package zka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const maxLocalPeerEnvironment = 64 << 10

var (
	errPeerPIDFDUnavailable = errors.New("kernel lacks SO_PEERPIDFD support (Linux 6.5 or newer is required)")
	errLocalPeerExited      = errors.New("local daemon peer exited before its session environment was captured")
)

type localPeerContextKey struct{}

type localPeerEnvironment struct {
	PID         int
	Environment map[string]string
	Err         error
}

func contextWithLocalPeer(ctx context.Context, conn net.Conn) context.Context {
	peer := localPeerEnvironment{}
	peer.PID, peer.Environment, peer.Err = readLocalPeerEnvironment(conn)
	return context.WithValue(ctx, localPeerContextKey{}, peer)
}

func localPeerFromContext(ctx context.Context) localPeerEnvironment {
	peer, _ := ctx.Value(localPeerContextKey{}).(localPeerEnvironment)
	if peer.Environment == nil && peer.Err == nil {
		peer.Err = errors.New("local peer session environment is unavailable")
	}
	return peer
}

func readLocalPeerEnvironment(conn net.Conn) (int, map[string]string, error) {
	if !sameUIDUnixPeer(conn, uint32(os.Getuid())) {
		return 0, nil, errors.New("local daemon peer UID does not match zkad")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, nil, errors.New("local daemon peer is not a Unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, nil, fmt.Errorf("access local daemon peer socket: %w", err)
	}
	var credential *unix.Ucred
	pidfd := -1
	var socketErr error
	var cloexecErr error
	syscall.ForkLock.RLock()
	controlErr := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if socketErr == nil {
			pidfd, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		}
		if socketErr == nil && pidfd >= 0 {
			flags, err := unix.FcntlInt(uintptr(pidfd), unix.F_GETFD, 0)
			if err == nil {
				_, err = unix.FcntlInt(uintptr(pidfd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
			}
			cloexecErr = err
		}
	})
	syscall.ForkLock.RUnlock()
	if controlErr != nil {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
		return 0, nil, fmt.Errorf("inspect local daemon peer: %w", controlErr)
	}
	if socketErr != nil {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
		if errors.Is(socketErr, unix.ENOPROTOOPT) || errors.Is(socketErr, unix.EINVAL) {
			return 0, nil, errPeerPIDFDUnavailable
		}
		if errors.Is(socketErr, unix.ESRCH) {
			return 0, nil, errLocalPeerExited
		}
		return 0, nil, fmt.Errorf("inspect local daemon peer: %w", socketErr)
	}
	if cloexecErr != nil {
		_ = unix.Close(pidfd)
		return 0, nil, fmt.Errorf("secure local daemon peer pidfd: %w", cloexecErr)
	}
	if credential == nil || credential.Pid <= 0 || pidfd < 0 {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
		return 0, nil, errors.New("local daemon peer credentials are incomplete")
	}
	defer unix.Close(pidfd)
	if !pidfdAlive(pidfd) {
		return int(credential.Pid), nil, errLocalPeerExited
	}

	procfd, err := unix.Open("/proc/"+strconv.Itoa(int(credential.Pid)), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return int(credential.Pid), nil, fmt.Errorf("open local peer proc directory: %w", err)
	}
	defer unix.Close(procfd)
	if !pidfdAlive(pidfd) {
		return int(credential.Pid), nil, errLocalPeerExited
	}
	envfd, err := unix.Openat(procfd, "environ", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return int(credential.Pid), nil, fmt.Errorf("read local peer environment: %w", err)
	}
	file := os.NewFile(uintptr(envfd), "peer-environ")
	if file == nil {
		_ = unix.Close(envfd)
		return int(credential.Pid), nil, errors.New("open local peer environment")
	}
	rawEnvironment, readErr := io.ReadAll(io.LimitReader(file, maxLocalPeerEnvironment+1))
	closeErr := file.Close()
	if readErr != nil {
		return int(credential.Pid), nil, fmt.Errorf("read local peer environment: %w", readErr)
	}
	if closeErr != nil {
		return int(credential.Pid), nil, fmt.Errorf("close local peer environment: %w", closeErr)
	}
	if len(rawEnvironment) > maxLocalPeerEnvironment {
		return int(credential.Pid), nil, fmt.Errorf("local peer environment exceeds %d bytes", maxLocalPeerEnvironment)
	}
	if !pidfdAlive(pidfd) {
		return int(credential.Pid), nil, errLocalPeerExited
	}
	environment, err := parseNULShellEnvironment(rawEnvironment)
	if err != nil {
		return int(credential.Pid), nil, err
	}
	return int(credential.Pid), environment, nil
}

func pidfdAlive(pidfd int) bool {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN | unix.POLLHUP}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n == 0
}

func parseNULShellEnvironment(raw []byte) (map[string]string, error) {
	environment := map[string]string{}
	for _, entry := range strings.Split(string(raw), "\x00") {
		if entry == "" {
			continue
		}
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("local peer environment is malformed")
		}
		environment[name] = value
	}
	return environment, nil
}
