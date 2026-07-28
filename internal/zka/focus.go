package zka

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// focusSwayWindow raises the compositor window owning a Kitty process. The
// command is passed in rather than hardcoded: zkad's PATH comes from the systemd
// unit, where a bare "swaymsg" does not resolve, so the module supplies an
// absolute store path.
func focusSwayWindow(ctx context.Context, runner CommandRunner, command string, pid int) error {
	if pid <= 0 || !swaySessionAvailable() {
		return nil
	}
	if command == "" {
		command = "swaymsg"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, _, err := runner.Run(ctx, command, fmt.Sprintf("[pid=%d] focus", pid)); err != nil {
		return fmt.Errorf("focus Sway window for Kitty process %d: %w", pid, err)
	}
	return nil
}

func swaySessionAvailable() bool {
	if strings.TrimSpace(os.Getenv("SWAYSOCK")) != "" {
		return true
	}
	// zkad starts from default.target and can beat Sway's environment import.
	// The per-user runtime socket is authoritative once the session is live.
	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		return false
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "sway-ipc.") || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return true
		}
	}
	return false
}
