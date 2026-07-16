package zka

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func focusSwayWindow(ctx context.Context, runner CommandRunner, pid int) error {
	if pid <= 0 || strings.TrimSpace(os.Getenv("SWAYSOCK")) == "" {
		return nil
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, _, err := runner.Run(ctx, "swaymsg", fmt.Sprintf("[pid=%d] focus", pid)); err != nil {
		return fmt.Errorf("focus Sway window for Kitty process %d: %w", pid, err)
	}
	return nil
}
