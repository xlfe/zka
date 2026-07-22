package zka

import (
	"fmt"
	"os"
	"path/filepath"
)

type Paths struct {
	StateDir      string
	RuntimeDir    string
	StateFile     string
	GeneratedDir  string
	AttachmentDir string
	AgentDir      string
	Socket        string
	WatcherSocket string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("find home directory: %w", err)
	}
	stateDir := os.Getenv("ZKA_STATE_DIR")
	if stateDir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "state")
		}
		stateDir = filepath.Join(base, "zka")
	}
	runtimeDir := os.Getenv("ZKA_RUNTIME_DIR")
	if runtimeDir == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			return Paths{}, fmt.Errorf("XDG_RUNTIME_DIR is not set (or set ZKA_RUNTIME_DIR)")
		}
		runtimeDir = filepath.Join(base, "zka")
	}
	socket := os.Getenv("ZKA_SOCKET")
	if socket == "" {
		socket = filepath.Join(runtimeDir, "zka.sock")
	}
	for label, path := range map[string]string{"state directory": stateDir, "runtime directory": runtimeDir, "socket": socket} {
		if !filepath.IsAbs(path) {
			return Paths{}, fmt.Errorf("%s must be absolute: %s", label, path)
		}
	}
	return Paths{
		StateDir:      stateDir,
		RuntimeDir:    runtimeDir,
		StateFile:     filepath.Join(stateDir, "state.json"),
		GeneratedDir:  filepath.Join(stateDir, "generated"),
		AttachmentDir: filepath.Join(runtimeDir, "kitty"),
		AgentDir:      filepath.Join(runtimeDir, "agents"),
		Socket:        socket,
		WatcherSocket: filepath.Join(runtimeDir, "watcher.sock"),
	}, nil
}
