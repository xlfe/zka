package zka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	swaymsgHelperEnv            = "ZKA_SWAYMSG_HELPER"
	swaymsgHelperSocketEnv      = "ZKA_SWAYMSG_HELPER_SOCKET"
	swaymsgHelperMessageEnv     = "ZKA_SWAYMSG_HELPER_MESSAGE"
	swaymsgHelperExpectedSource = "XDG_RUNTIME_DIR"
)

func TestMain(m *testing.M) {
	if os.Getenv(swaymsgHelperEnv) == "1" {
		want := []string{
			"--socket",
			os.Getenv(swaymsgHelperSocketEnv),
			os.Getenv(swaymsgHelperMessageEnv),
		}
		if !reflect.DeepEqual(os.Args[1:], want) {
			fmt.Fprintf(os.Stderr, "swaymsg helper args = %#v, want %#v\n", os.Args[1:], want)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFocusSwayWindowUsesKittyProcessID(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
	t.Setenv("I3SOCK", "")
	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "swaymsg", 635439); err != nil {
		t.Fatal(err)
	}
	want := []runnerCall{{
		Name: "swaymsg",
		Args: []string{"--socket", "/run/user/1234/sway-ipc.sock", "[pid=635439] focus"},
	}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestFocusSwayWindowSkipsNonSwaySession(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if err := os.WriteFile(filepath.Join(runtimeDir, "sway-ipc.1000.41.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "swaymsg", 635439); err != nil {
		t.Fatal(err)
	}
	if got := runner.Calls(); len(got) != 0 {
		t.Fatalf("calls = %#v", got)
	}
}

func TestFocusSwayWindowDiscoversSocketCreatedAfterDaemonStart(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	socket := filepath.Join(runtimeDir, "sway-ipc.1000.42.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "swaymsg", 635439); err != nil {
		t.Fatal(err)
	}
	want := []runnerCall{{
		Name: "swaymsg",
		Args: []string{"--socket", socket, "[pid=635439] focus"},
	}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestFocusSwayWindowExecRunnerReceivesDiscoveredSocket(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	socket := filepath.Join(runtimeDir, "sway-ipc.1000.43.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(swaymsgHelperEnv, "1")
	t.Setenv(swaymsgHelperSocketEnv, socket)
	t.Setenv(swaymsgHelperMessageEnv, "[pid=635439] focus")
	if err := focusSwayWindow(context.Background(), nil, executable, 635439); err != nil {
		t.Fatal(err)
	}
}

func TestFocusSwayWindowReportsCompositorFailure(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
	t.Setenv("I3SOCK", "")
	runner := &fakeRunner{handler: func(context.Context, string, ...string) (string, string, error) {
		return "", "", errors.New("focus rejected")
	}}
	if err := focusSwayWindow(context.Background(), runner, "swaymsg", 42); err == nil {
		t.Fatal("expected focus failure")
	}
}

// The daemon's PATH comes from the systemd unit, where a bare "swaymsg" does not
// resolve. The configured absolute path must be the one actually executed.
func TestFocusSwayWindowUsesConfiguredCommand(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
	t.Setenv("I3SOCK", "")
	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "/nix/store/abc-sway/bin/swaymsg", 7); err != nil {
		t.Fatal(err)
	}
	want := []runnerCall{{
		Name: "/nix/store/abc-sway/bin/swaymsg",
		Args: []string{"--socket", "/run/user/1234/sway-ipc.sock", "[pid=7] focus"},
	}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestRunSwayCommandLiveHintAvoidsRuntimeScan(t *testing.T) {
	values := map[string]string{
		"SWAYSOCK":        " /run/user/1234/sway.sock ",
		"I3SOCK":          "/run/user/1234/i3.sock",
		"XDG_RUNTIME_DIR": "/run/user/1234",
	}
	readDirCalls := 0
	runner := &fakeRunner{}
	socket, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] },
		func(string) ([]os.DirEntry, error) {
			readDirCalls++
			return nil, errors.New("unexpected runtime scan")
		}, defaultSwayCommandTimeouts, "--type", "get_version")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != "/run/user/1234/sway.sock" || socket.Source != "SWAYSOCK" || len(socket.FailedAttempts) != 0 {
		t.Fatalf("socket = %#v", socket)
	}
	if readDirCalls != 0 {
		t.Fatalf("runtime scans = %d, want 0", readDirCalls)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0].Args[1] != socket.Path {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestRunSwayCommandFallsBackFromStaleSwayToI3(t *testing.T) {
	values := map[string]string{
		"SWAYSOCK":        " /run/user/1234/stale-sway.sock ",
		"I3SOCK":          " /run/user/1234/i3.sock ",
		"XDG_RUNTIME_DIR": "/run/user/1234",
	}
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if args[1] == "/run/user/1234/stale-sway.sock" {
			return "", "", errors.New("unable to connect")
		}
		return "", "", nil
	}}
	socket, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] },
		func(string) ([]os.DirEntry, error) { return nil, errors.New("unexpected runtime scan") },
		defaultSwayCommandTimeouts, "--type", "get_version")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != "/run/user/1234/i3.sock" || socket.Source != "I3SOCK" || len(socket.FailedAttempts) != 1 || socket.FailedAttempts[0].Source != "SWAYSOCK" {
		t.Fatalf("socket = %#v", socket)
	}
	if calls := runner.Calls(); len(calls) != 2 {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestRunSwayCommandRuntimeFallbackIsNewestFirstAndCrossPhaseDeduplicated(t *testing.T) {
	runtimeDir := testRoot(t)
	hintPath := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.10.sock", time.Unix(30, 0))
	olderLive := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.11.sock", time.Unix(10, 0))
	newerDead := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.12.sock", time.Unix(20, 0))
	if err := os.WriteFile(filepath.Join(runtimeDir, "sway-ipc.1000.regular.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{"SWAYSOCK": hintPath, "XDG_RUNTIME_DIR": runtimeDir}
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		switch args[1] {
		case hintPath, newerDead:
			return "", "", errors.New("unable to connect")
		case olderLive:
			return "", "", nil
		default:
			return "", "", fmt.Errorf("unexpected socket %s", args[1])
		}
	}}
	socket, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] }, os.ReadDir,
		defaultSwayCommandTimeouts, "--type", "get_version")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != olderLive || socket.Source != "XDG_RUNTIME_DIR" || len(socket.FailedAttempts) != 2 {
		t.Fatalf("socket = %#v", socket)
	}
	wantPaths := []string{hintPath, newerDead, olderLive}
	for index, call := range runner.Calls() {
		if index >= len(wantPaths) || call.Args[1] != wantPaths[index] {
			t.Fatalf("calls = %#v, want paths %#v", runner.Calls(), wantPaths)
		}
	}
	if len(runner.Calls()) != len(wantPaths) {
		t.Fatalf("calls = %#v, want paths %#v", runner.Calls(), wantPaths)
	}
}

func TestRunSwayCommandBoundsEveryAttemptWithoutCallerDeadline(t *testing.T) {
	values := map[string]string{
		"SWAYSOCK": "/run/user/1234/sway.sock",
		"I3SOCK":   "/run/user/1234/i3.sock",
	}
	var remaining []time.Duration
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, args ...string) (string, string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return "", "", errors.New("attempt has no deadline")
		}
		remaining = append(remaining, time.Until(deadline))
		if args[1] == values["SWAYSOCK"] {
			return "", "", errors.New("unable to connect")
		}
		return "", "", nil
	}}
	timeouts := swayCommandTimeouts{Overall: 300 * time.Millisecond, Primary: 200 * time.Millisecond, Fallback: 50 * time.Millisecond}
	if _, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] },
		func(string) ([]os.DirEntry, error) { return nil, nil },
		timeouts, "--type", "get_version"); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0] < 150*time.Millisecond || remaining[1] > 75*time.Millisecond {
		t.Fatalf("attempt deadlines = %v", remaining)
	}
}

func TestRunSwayCommandFirstRuntimeCandidateGetsPrimaryBudget(t *testing.T) {
	runtimeDir := testRoot(t)
	runtimeSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.19.sock", time.Now())
	values := map[string]string{"XDG_RUNTIME_DIR": runtimeDir}
	var remaining time.Duration
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, args ...string) (string, string, error) {
		if args[1] != runtimeSocket {
			return "", "", fmt.Errorf("unexpected socket %s", args[1])
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			return "", "", errors.New("attempt has no deadline")
		}
		remaining = time.Until(deadline)
		return "", "", nil
	}}
	timeouts := swayCommandTimeouts{Overall: 300 * time.Millisecond, Primary: 200 * time.Millisecond, Fallback: 50 * time.Millisecond}
	if _, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] }, os.ReadDir,
		timeouts, "--type", "get_version"); err != nil {
		t.Fatal(err)
	}
	if remaining < 150*time.Millisecond {
		t.Fatalf("first runtime candidate deadline = %s, want primary budget", remaining)
	}
}

func TestRunSwayCommandPreservesSuccessAtAttemptDeadline(t *testing.T) {
	values := map[string]string{"SWAYSOCK": "/run/user/1234/sway.sock"}
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, _ ...string) (string, string, error) {
		<-ctx.Done()
		return "", "", nil
	}}
	timeouts := swayCommandTimeouts{Overall: 100 * time.Millisecond, Primary: 5 * time.Millisecond, Fallback: 5 * time.Millisecond}
	socket, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] },
		func(string) ([]os.DirEntry, error) { return nil, nil },
		timeouts, "--type", "get_version")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != values["SWAYSOCK"] || socket.Source != "SWAYSOCK" || len(socket.FailedAttempts) != 0 {
		t.Fatalf("socket = %#v", socket)
	}
}

func TestRunSwayCommandHungFallbackDoesNotStarveRuntimeCandidate(t *testing.T) {
	runtimeDir := testRoot(t)
	runtimeSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.20.sock", time.Now())
	values := map[string]string{
		"SWAYSOCK": "/run/user/1234/stale.sock", "I3SOCK": "/run/user/1234/hung.sock",
		"XDG_RUNTIME_DIR": runtimeDir,
	}
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, args ...string) (string, string, error) {
		switch args[1] {
		case values["SWAYSOCK"]:
			return "", "", errors.New("unable to connect")
		case values["I3SOCK"]:
			<-ctx.Done()
			return "", "", ctx.Err()
		case runtimeSocket:
			return "", "", nil
		default:
			return "", "", fmt.Errorf("unexpected socket %s", args[1])
		}
	}}
	timeouts := swayCommandTimeouts{Overall: 200 * time.Millisecond, Primary: 100 * time.Millisecond, Fallback: 10 * time.Millisecond}
	socket, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] }, os.ReadDir,
		timeouts, "--type", "get_version")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != runtimeSocket || len(socket.FailedAttempts) != 2 || !strings.Contains(socket.FailedAttempts[1].Error, "deadline exceeded") {
		t.Fatalf("socket = %#v", socket)
	}
}

func TestRunSwayCommandDoesNotCacheRecoveredSocket(t *testing.T) {
	runtimeDir := testRoot(t)
	runtimeSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.30.sock", time.Now())
	values := map[string]string{"SWAYSOCK": "/run/user/1234/stale.sock", "XDG_RUNTIME_DIR": runtimeDir}
	scans := 0
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if args[1] == values["SWAYSOCK"] {
			return "", "", errors.New("unable to connect")
		}
		if args[1] == runtimeSocket {
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected socket %s", args[1])
	}}
	readDir := func(path string) ([]os.DirEntry, error) {
		scans++
		return os.ReadDir(path)
	}
	for range 2 {
		if _, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
			func(name string) string { return values[name] }, readDir,
			defaultSwayCommandTimeouts, "--type", "get_version"); err != nil {
			t.Fatal(err)
		}
	}
	if scans != 2 || len(runner.Calls()) != 4 {
		t.Fatalf("runtime scans = %d, calls = %#v", scans, runner.Calls())
	}
}

func TestRunSwayCommandReportsAllFailedCandidates(t *testing.T) {
	runtimeDir := testRoot(t)
	runtimeSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.40.sock", time.Now())
	values := map[string]string{"SWAYSOCK": "/run/user/1234/stale.sock", "XDG_RUNTIME_DIR": runtimeDir}
	runner := &fakeRunner{handler: func(context.Context, string, ...string) (string, string, error) {
		return "", "", errors.New("unable to connect")
	}}
	_, err := runSwayCommandWith(context.Background(), runner, "swaymsg-test",
		func(name string) string { return values[name] }, os.ReadDir,
		defaultSwayCommandTimeouts, "--type", "get_version")
	if err == nil || !strings.Contains(err.Error(), values["SWAYSOCK"]) || !strings.Contains(err.Error(), runtimeSocket) {
		t.Fatalf("error = %v", err)
	}
}

func TestProbeSwayIPCUsesResolvedSocket(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
	t.Setenv("I3SOCK", "")
	runner := &fakeRunner{}
	socket, err := probeSwayIPC(context.Background(), runner, "swaymsg-test")
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != "/run/user/1234/sway-ipc.sock" || socket.Source != "SWAYSOCK" {
		t.Fatalf("socket = %#v", socket)
	}
	want := []runnerCall{{
		Name: "swaymsg-test",
		Args: []string{"--socket", "/run/user/1234/sway-ipc.sock", "--type", "get_version"},
	}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestProbeSwayIPCReportsMissingSocket(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	runner := &fakeRunner{}
	if _, err := probeSwayIPC(context.Background(), runner, "swaymsg-test"); err == nil {
		t.Fatal("expected missing socket failure")
	}
	if got := runner.Calls(); len(got) != 0 {
		t.Fatalf("calls = %#v", got)
	}
}

func TestProbeSwayIPCRuntimeRecoveryWithoutHintsIsNotDoctorWarning(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	liveSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.60.sock", time.Unix(10, 0))
	deadSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.61.sock", time.Unix(20, 0))
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if args[1] == deadSocket {
			return "", "", errors.New("unable to connect")
		}
		if args[1] == liveSocket {
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected socket %s", args[1])
	}}
	check := swayIPCDoctorCheck(context.Background(), false, true, func(ctx context.Context) (swaySocketInfo, error) {
		return probeSwayIPC(ctx, runner, "swaymsg-test")
	})
	if !check.OK || check.Warning || check.Detail != liveSocket+" via XDG_RUNTIME_DIR" {
		t.Fatalf("check = %#v", check)
	}
}

func TestDaemonSwayIPCRoundTripUsesDaemonRunner(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	socketPath := filepath.Join(runtimeDir, "sway-ipc.1000.45.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runner := &fakeRunner{}
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Focus.SwayCommand = "swaymsg-daemon"
	serveTestDaemon(t, d)

	socket, err := NewAPI(d.paths).SwayIPC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != socketPath || socket.Source != "XDG_RUNTIME_DIR" {
		t.Fatalf("socket = %#v", socket)
	}
	want := []runnerCall{{
		Name: "swaymsg-daemon",
		Args: []string{"--socket", socketPath, "--type", "get_version"},
	}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestDaemonSwayIPCRoundTripCarriesFailedHintMetadata(t *testing.T) {
	runtimeDir := testRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	staleHint := filepath.Join(runtimeDir, "sway-ipc.1000.50.sock")
	t.Setenv("SWAYSOCK", staleHint)
	t.Setenv("I3SOCK", "")
	activeSocket := listenSwayTestSocket(t, runtimeDir, "sway-ipc.1000.51.sock", time.Now())

	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		if args[1] == staleHint {
			return "", "", errors.New("unable to connect")
		}
		if args[1] == activeSocket {
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected socket %s", args[1])
	}}
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Focus.SwayCommand = "swaymsg-daemon"
	serveTestDaemon(t, d)

	socket, err := NewAPI(d.paths).SwayIPC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if socket.Path != activeSocket || socket.Source != "XDG_RUNTIME_DIR" || len(socket.FailedAttempts) != 1 {
		t.Fatalf("socket = %#v", socket)
	}
	failed := socket.FailedAttempts[0]
	if failed.Path != staleHint || failed.Source != "SWAYSOCK" || !strings.Contains(failed.Error, "unable to connect") {
		t.Fatalf("failed hint = %#v", failed)
	}
}

func BenchmarkDiscoverRuntimeSwaySockets(b *testing.B) {
	b.Run("runtime-directory", func(b *testing.B) {
		runtimeDir := b.TempDir()
		socketPath := filepath.Join(runtimeDir, "sway-ipc.1000.44.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			b.Fatal(err)
		}
		defer listener.Close()
		getenv := func(name string) string {
			if name == "XDG_RUNTIME_DIR" {
				return runtimeDir
			}
			return ""
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sockets, err := discoverRuntimeSwaySockets(getenv, os.ReadDir, map[string]bool{})
			if err != nil || len(sockets) != 1 || sockets[0].Path != socketPath || sockets[0].Source != swaymsgHelperExpectedSource {
				b.Fatalf("sockets = %#v, err = %v", sockets, err)
			}
		}
	})

	b.Run("runtime-directory-256-entries", func(b *testing.B) {
		runtimeDir := b.TempDir()
		for i := 0; i < 255; i++ {
			path := filepath.Join(runtimeDir, fmt.Sprintf("service-%03d", i))
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				b.Fatal(err)
			}
		}
		socketPath := filepath.Join(runtimeDir, "sway-ipc.1000.46.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			b.Fatal(err)
		}
		defer listener.Close()
		getenv := func(name string) string {
			if name == "XDG_RUNTIME_DIR" {
				return runtimeDir
			}
			return ""
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sockets, err := discoverRuntimeSwaySockets(getenv, os.ReadDir, map[string]bool{})
			if err != nil || len(sockets) != 1 || sockets[0].Path != socketPath || sockets[0].Source != swaymsgHelperExpectedSource {
				b.Fatalf("sockets = %#v, err = %v", sockets, err)
			}
		}
	})
}

func listenSwayTestSocket(t testing.TB, dir, name string, boundAt time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chtimes(path, boundAt, boundAt); err != nil {
		t.Fatal(err)
	}
	return path
}
