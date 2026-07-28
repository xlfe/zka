package zka

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFocusSwayWindowUsesKittyProcessID(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "swaymsg", 635439); err != nil {
		t.Fatal(err)
	}
	want := []runnerCall{{Name: "swaymsg", Args: []string{"[pid=635439] focus"}}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestFocusSwayWindowSkipsNonSwaySession(t *testing.T) {
	t.Setenv("SWAYSOCK", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
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
	want := []runnerCall{{Name: "swaymsg", Args: []string{"[pid=635439] focus"}}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestFocusSwayWindowReportsCompositorFailure(t *testing.T) {
	t.Setenv("SWAYSOCK", "/run/user/1234/sway-ipc.sock")
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
	runner := &fakeRunner{}
	if err := focusSwayWindow(context.Background(), runner, "/nix/store/abc-sway/bin/swaymsg", 7); err != nil {
		t.Fatal(err)
	}
	want := []runnerCall{{Name: "/nix/store/abc-sway/bin/swaymsg", Args: []string{"[pid=7] focus"}}}
	if got := runner.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}
