package zka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type pivbCapabilities struct {
	Schema                int      `json:"schema"`
	AttachmentProtocols   []int    `json:"attachment_protocols"`
	AttachmentModes       []string `json:"attachment_modes"`
	RouteBindingProtocols []string `json:"route_binding_protocols"`
}

type pivbCapabilityCacheEntry struct {
	key          string
	capabilities pivbCapabilities
	version      string
}

var managedPIVBCapabilityCache struct {
	sync.Mutex
	entries map[string]pivbCapabilityCacheEntry
}

func ensureManagedPIVBCapability(ctx context.Context, cfg Config, runner CommandRunner) error {
	if !configHasPIVBBundle(cfg) {
		return nil
	}
	command, key, err := resolvedPIVBCommand(cfg.Credentials.PIVB.Command, runner)
	if err != nil {
		return err
	}
	managedPIVBCapabilityCache.Lock()
	entry, cached := managedPIVBCapabilityCache.entries[key]
	managedPIVBCapabilityCache.Unlock()
	cached = cached && key != ""
	if !cached {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		stdout, stderr, probeErr := runner.Run(probeCtx, command, "capabilities", "--format=json")
		cancel()
		versionCtx, versionCancel := context.WithTimeout(ctx, 2*time.Second)
		versionOut, _, _ := runner.Run(versionCtx, command, "version")
		versionCancel()
		if probeErr != nil {
			return fmt.Errorf("PIVB %s does not support managed attachment protocol: %v (%s); upgrade PIVB before recreating this backend", strings.TrimSpace(versionOut), probeErr, strings.TrimSpace(stderr))
		}
		// Capability envelopes deliberately permit future fields.
		decoder := json.NewDecoder(strings.NewReader(stdout))
		if err := decoder.Decode(&entry.capabilities); err != nil {
			return fmt.Errorf("PIVB %s returned invalid capabilities: %w", strings.TrimSpace(versionOut), err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return fmt.Errorf("PIVB %s returned multiple capability documents", strings.TrimSpace(versionOut))
		}
		entry.key, entry.version = key, strings.TrimSpace(versionOut)
		if key != "" {
			managedPIVBCapabilityCache.Lock()
			if managedPIVBCapabilityCache.entries == nil {
				managedPIVBCapabilityCache.entries = map[string]pivbCapabilityCacheEntry{}
			}
			managedPIVBCapabilityCache.entries[key] = entry
			managedPIVBCapabilityCache.Unlock()
		}
	}
	requiredProtocol := managedPIVBAttachmentProtocol(cfg)
	if entry.capabilities.Schema != 1 || !containsInt(entry.capabilities.AttachmentProtocols, requiredProtocol) ||
		!containsString(entry.capabilities.AttachmentModes, "route-required") {
		return fmt.Errorf("PIVB %s at %s lacks attachment protocol %d route-required support (schema=%d protocols=%v modes=%v); upgrade PIVB before recreating this backend",
			entry.version, command, requiredProtocol, entry.capabilities.Schema, entry.capabilities.AttachmentProtocols, entry.capabilities.AttachmentModes)
	}
	return nil
}

// ensurePaneVisiblePIVBCapability checks the executable selected by the
// launching user's PATH independently of zkad's configured/package-pinned
// executable. A package probe alone is insufficient when a pane can resolve a
// stale profile or direnv binary.
func ensurePaneVisiblePIVBCapability(ctx context.Context, cfg Config) error {
	if !configHasPIVBBundle(cfg) {
		return nil
	}
	paneConfig := cfg
	paneConfig.Credentials.PIVB.Command = "pivb"
	if err := ensureManagedPIVBCapability(ctx, paneConfig, ExecRunner{}); err != nil {
		return fmt.Errorf("pane-visible PIVB from PATH: %w", err)
	}
	return nil
}

func ensureManagedPIVBLaunchCapabilities(ctx context.Context, cfg Config, runner CommandRunner) error {
	if err := ensureManagedPIVBCapability(ctx, cfg, runner); err != nil {
		return err
	}
	if !configHasPIVBBundle(cfg) {
		return nil
	}
	pathConfig := cfg
	pathConfig.Credentials.PIVB.Command = "pivb"
	if err := ensureManagedPIVBCapability(ctx, pathConfig, runner); err != nil {
		return fmt.Errorf("pane-launch PATH PIVB: %w", err)
	}
	return nil
}

func resolvedPIVBCommand(command string, runner CommandRunner) (string, string, error) {
	if command == "" {
		return "", "", fmt.Errorf("credentials.pivb.command is empty")
	}
	resolved := command
	if _, production := runner.(ExecRunner); production {
		var err error
		resolved, err = exec.LookPath(command)
		if err != nil {
			return "", "", fmt.Errorf("resolve managed PIVB executable %q: %w", command, err)
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		// Test runners and injected command seams need not name host files. They
		// still get per-command caching and full capability validation.
		return resolved, "", nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return resolved, "path:" + resolved + ":" + info.ModTime().UTC().Format(time.RFC3339Nano), nil
	}
	key := strings.Join([]string{
		resolved, strconv.FormatUint(uint64(stat.Dev), 10), strconv.FormatUint(stat.Ino, 10),
		strconv.FormatInt(info.Size(), 10), strconv.FormatInt(info.ModTime().UnixNano(), 10),
	}, ":")
	return resolved, key, nil
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
