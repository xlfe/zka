package zka

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	swayCommandOverallTimeout = 3 * time.Second
	swayPrimaryAttemptTimeout = 2 * time.Second
	swayFallbackTimeout       = 500 * time.Millisecond
)

var errNoSwayIPCSocket = errors.New("no Sway IPC socket available to zkad")

type swaySocketAttempt struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	Error  string `json:"error"`
}

type swaySocketInfo struct {
	Path           string              `json:"path"`
	Source         string              `json:"source"`
	FailedAttempts []swaySocketAttempt `json:"failed_attempts,omitempty"`
}

type swaySocketCandidate struct {
	Path    string
	Source  string
	BoundAt time.Time
}

type swayCommandTimeouts struct {
	Overall  time.Duration
	Primary  time.Duration
	Fallback time.Duration
}

var defaultSwayCommandTimeouts = swayCommandTimeouts{
	Overall: swayCommandOverallTimeout, Primary: swayPrimaryAttemptTimeout, Fallback: swayFallbackTimeout,
}

// focusSwayWindow raises the compositor window owning a Kitty process. The
// command is passed in rather than hardcoded: zkad's PATH comes from the systemd
// unit, where a bare "swaymsg" does not resolve, so the module supplies an
// absolute store path.
func focusSwayWindow(ctx context.Context, runner CommandRunner, command string, pid int) error {
	if pid <= 0 {
		return nil
	}
	_, err := runSwayCommand(ctx, runner, command, fmt.Sprintf("[pid=%d] focus", pid))
	if errors.Is(err, errNoSwayIPCSocket) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("focus Sway window for Kitty process %d: %w", pid, err)
	}
	return nil
}

func probeSwayIPC(ctx context.Context, runner CommandRunner, command string) (swaySocketInfo, error) {
	socket, err := runSwayCommand(ctx, runner, command, "--type", "get_version")
	if err != nil {
		return swaySocketInfo{}, fmt.Errorf("probe Sway IPC: %w", err)
	}
	return socket, nil
}

func runSwayCommand(ctx context.Context, runner CommandRunner, command string, operationArgs ...string) (swaySocketInfo, error) {
	return runSwayCommandWith(ctx, runner, command, os.Getenv, os.ReadDir, defaultSwayCommandTimeouts, operationArgs...)
}

func runSwayCommandWith(
	ctx context.Context,
	runner CommandRunner,
	command string,
	getenv func(string) string,
	readDir func(string) ([]os.DirEntry, error),
	timeouts swayCommandTimeouts,
	operationArgs ...string,
) (swaySocketInfo, error) {
	if command == "" {
		command = "swaymsg"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	overallCtx, overallCancel := context.WithTimeout(ctx, timeouts.Overall)
	defer overallCancel()

	seen := map[string]bool{}
	failed := make([]swaySocketAttempt, 0, 4)
	primaryAttempt := true
	attempt := func(candidate swaySocketCandidate) (swaySocketInfo, bool) {
		path := filepath.Clean(strings.TrimSpace(candidate.Path))
		if path == "." || seen[path] {
			return swaySocketInfo{}, false
		}
		seen[path] = true
		timeout := timeouts.Fallback
		if primaryAttempt {
			timeout = timeouts.Primary
			primaryAttempt = false
		}
		attemptCtx, attemptCancel := context.WithTimeout(overallCtx, timeout)
		args := []string{"--socket", path}
		args = append(args, operationArgs...)
		_, _, err := runner.Run(attemptCtx, command, args...)
		// A command that completed successfully at the deadline still completed.
		// Prefer the context error only when the runner itself failed, otherwise a
		// healthy hint can become a false stale-socket warning.
		if err != nil && attemptCtx.Err() != nil {
			err = attemptCtx.Err()
		}
		attemptCancel()
		if err == nil {
			return swaySocketInfo{
				Path: path, Source: candidate.Source,
				FailedAttempts: append([]swaySocketAttempt(nil), failed...),
			}, true
		}
		failed = append(failed, swaySocketAttempt{Path: path, Source: candidate.Source, Error: err.Error()})
		return swaySocketInfo{}, false
	}

	for _, variable := range []string{"SWAYSOCK", "I3SOCK"} {
		path := strings.TrimSpace(getenv(variable))
		if path == "" {
			continue
		}
		if socket, ok := attempt(swaySocketCandidate{Path: path, Source: variable}); ok {
			return socket, nil
		}
		if overallCtx.Err() != nil {
			return swaySocketInfo{}, swayCommandFailure(failed, nil)
		}
	}

	candidates, discoveryErr := discoverRuntimeSwaySockets(getenv, readDir, seen)
	for _, candidate := range candidates {
		if socket, ok := attempt(candidate); ok {
			return socket, nil
		}
		if overallCtx.Err() != nil {
			break
		}
	}
	if len(failed) == 0 && discoveryErr == nil {
		return swaySocketInfo{}, errNoSwayIPCSocket
	}
	return swaySocketInfo{}, swayCommandFailure(failed, discoveryErr)
}

func discoverRuntimeSwaySockets(
	getenv func(string) string,
	readDir func(string) ([]os.DirEntry, error),
	alreadyTried map[string]bool,
) ([]swaySocketCandidate, error) {
	runtimeDir := strings.TrimSpace(getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		return nil, nil
	}
	entries, err := readDir(runtimeDir)
	if err != nil {
		return nil, fmt.Errorf("scan XDG_RUNTIME_DIR %s: %w", runtimeDir, err)
	}
	candidates := make([]swaySocketCandidate, 0, 2)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "sway-ipc.") || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		path := filepath.Clean(filepath.Join(runtimeDir, entry.Name()))
		// Deduplicate across phases. A stale SWAYSOCK commonly remains as a
		// socket entry after Sway restarts; retrying it wastes a fallback slot.
		if alreadyTried[path] {
			continue
		}
		candidates = append(candidates, swaySocketCandidate{
			Path: path, Source: "XDG_RUNTIME_DIR", BoundAt: info.ModTime(),
		})
	}
	// A Unix socket's mtime is set when it is bound, so newest-first normally
	// selects the compositor that replaced a stale environment hint. This is
	// deterministic and more useful than os.ReadDir's alphabetical order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].BoundAt.Equal(candidates[j].BoundAt) {
			return candidates[i].Path < candidates[j].Path
		}
		return candidates[i].BoundAt.After(candidates[j].BoundAt)
	})
	return candidates, nil
}

func swayCommandFailure(attempts []swaySocketAttempt, discoveryErr error) error {
	parts := make([]string, 0, len(attempts)+1)
	for _, attempt := range attempts {
		parts = append(parts, fmt.Sprintf("%s=%s: %s", attempt.Source, attempt.Path, attempt.Error))
	}
	if discoveryErr != nil {
		parts = append(parts, discoveryErr.Error())
	}
	if len(parts) == 0 {
		return errNoSwayIPCSocket
	}
	return fmt.Errorf("no reachable Sway IPC socket: %s", strings.Join(parts, "; "))
}
