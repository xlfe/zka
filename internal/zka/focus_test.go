package zka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	runtimeDir := t.TempDir()
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
	runtimeDir := t.TempDir()
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
	runtimeDir := t.TempDir()
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

func TestResolveSwaySocketEnvironmentPriorityAvoidsRuntimeScan(t *testing.T) {
	values := map[string]string{
		"SWAYSOCK":        " /run/user/1234/sway.sock ",
		"I3SOCK":          "/run/user/1234/i3.sock",
		"XDG_RUNTIME_DIR": "/run/user/1234",
	}
	readDirCalls := 0
	socket, ok := resolveSwaySocketWith(func(name string) string {
		return values[name]
	}, func(string) ([]os.DirEntry, error) {
		readDirCalls++
		return nil, errors.New("unexpected runtime scan")
	})
	if !ok {
		t.Fatal("socket not resolved")
	}
	if socket.Path != "/run/user/1234/sway.sock" || socket.Source != "SWAYSOCK" {
		t.Fatalf("socket = %#v", socket)
	}
	if readDirCalls != 0 {
		t.Fatalf("runtime scans = %d, want 0", readDirCalls)
	}
}

func TestResolveSwaySocketUsesI3Socket(t *testing.T) {
	values := map[string]string{
		"I3SOCK":          " /run/user/1234/i3.sock ",
		"XDG_RUNTIME_DIR": "/run/user/1234",
	}
	socket, ok := resolveSwaySocketWith(func(name string) string {
		return values[name]
	}, func(string) ([]os.DirEntry, error) {
		return nil, errors.New("unexpected runtime scan")
	})
	if !ok {
		t.Fatal("socket not resolved")
	}
	if socket.Path != "/run/user/1234/i3.sock" || socket.Source != "I3SOCK" {
		t.Fatalf("socket = %#v", socket)
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

func TestDaemonSwayIPCRoundTripUsesDaemonRunner(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	socketPath := filepath.Join(runtimeDir, "sway-ipc.1000.45.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runner := &fakeRunner{}
	d, err := newTestDaemon(t, t.TempDir(), runner)
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

func BenchmarkResolveSwaySocket(b *testing.B) {
	b.Run("environment", func(b *testing.B) {
		b.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
		b.Setenv("I3SOCK", "")
		b.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, ok := resolveSwaySocket(); !ok {
				b.Fatal("socket not resolved")
			}
		}
	})

	b.Run("runtime-directory", func(b *testing.B) {
		runtimeDir := b.TempDir()
		socketPath := filepath.Join(runtimeDir, "sway-ipc.1000.44.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			b.Fatal(err)
		}
		defer listener.Close()
		b.Setenv("SWAYSOCK", "")
		b.Setenv("I3SOCK", "")
		b.Setenv("XDG_RUNTIME_DIR", runtimeDir)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			socket, ok := resolveSwaySocket()
			if !ok || socket.Path != socketPath || socket.Source != swaymsgHelperExpectedSource {
				b.Fatalf("socket = %#v, ok = %v", socket, ok)
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
		b.Setenv("SWAYSOCK", "")
		b.Setenv("I3SOCK", "")
		b.Setenv("XDG_RUNTIME_DIR", runtimeDir)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			socket, ok := resolveSwaySocket()
			if !ok || socket.Path != socketPath || socket.Source != swaymsgHelperExpectedSource {
				b.Fatalf("socket = %#v, ok = %v", socket, ok)
			}
		}
	})
}
