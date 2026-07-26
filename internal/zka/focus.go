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
	if pid <= 0 || strings.TrimSpace(os.Getenv("SWAYSOCK")) == "" {
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
