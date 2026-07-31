package zka

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type swaySocketInfo struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// focusSwayWindow raises the compositor window owning a Kitty process. The
// command is passed in rather than hardcoded: zkad's PATH comes from the systemd
// unit, where a bare "swaymsg" does not resolve, so the module supplies an
// absolute store path.
func focusSwayWindow(ctx context.Context, runner CommandRunner, command string, pid int) error {
	if pid <= 0 {
		return nil
	}
	socket, ok := resolveSwaySocket()
	if !ok {
		return nil
	}
	if command == "" {
		command = "swaymsg"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, _, err := runner.Run(ctx, command, "--socket", socket.Path, fmt.Sprintf("[pid=%d] focus", pid)); err != nil {
		return fmt.Errorf("focus Sway window for Kitty process %d via %s (%s): %w", pid, socket.Path, socket.Source, err)
	}
	return nil
}

func probeSwayIPC(ctx context.Context, runner CommandRunner, command string) (swaySocketInfo, error) {
	socket, ok := resolveSwaySocket()
	if !ok {
		return swaySocketInfo{}, fmt.Errorf("no Sway IPC socket available to zkad")
	}
	if command == "" {
		command = "swaymsg"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, _, err := runner.Run(ctx, command, "--socket", socket.Path, "--type", "get_version"); err != nil {
		return swaySocketInfo{}, fmt.Errorf("probe Sway IPC via %s (%s): %w", socket.Path, socket.Source, err)
	}
	return socket, nil
}

func resolveSwaySocket() (swaySocketInfo, bool) {
	return resolveSwaySocketWith(os.Getenv, os.ReadDir)
}

func resolveSwaySocketWith(
	getenv func(string) string,
	readDir func(string) ([]os.DirEntry, error),
) (swaySocketInfo, bool) {
	for _, variable := range []string{"SWAYSOCK", "I3SOCK"} {
		if path := strings.TrimSpace(getenv(variable)); path != "" {
			return swaySocketInfo{Path: path, Source: variable}, true
		}
	}
	// zkad starts from default.target and can beat Sway's environment import.
	// Resolve the per-user runtime socket on every action so a socket created
	// after daemon startup becomes visible without restarting zkad.
	runtimeDir := strings.TrimSpace(getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		return swaySocketInfo{}, false
	}
	entries, err := readDir(runtimeDir)
	if err != nil {
		return swaySocketInfo{}, false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "sway-ipc.") || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return swaySocketInfo{
				Path:   filepath.Join(runtimeDir, entry.Name()),
				Source: "XDG_RUNTIME_DIR",
			}, true
		}
	}
	return swaySocketInfo{}, false
}
