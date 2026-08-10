package zka

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// Version 2 projected managed paths into every pane. Version 3 introduced
	// the accidental local/remote split. Version 4 restores one stable managed
	// environment for every newly created pane.
	legacyCredentialEnvironmentVersion = 2
	credentialEnvironmentVersion       = 4
	agentRelayDialTimeout              = 500 * time.Millisecond
)

// agentRelaySocketPath is stable across claim generations and transport
// reconnects, so a pane's SSH_AUTH_SOCK never depends on a particular SSH
// control process.
func agentRelaySocketPath(dir, workspaceID string) string {
	path := filepath.Join(dir, workspaceID+".sock")
	if len(path) <= safeUnixSocketPath {
		return path
	}
	digest := sha256.Sum256([]byte(workspaceID))
	encoded := hex.EncodeToString(digest[:])
	available := safeUnixSocketPath - len(dir) - len(string(os.PathSeparator)) - len(".sock")
	if available < 8 {
		return path
	}
	if available > len(encoded) {
		available = len(encoded)
	}
	return filepath.Join(dir, encoded[:available]+".sock")
}

func dialAgentSocket(path string) (net.Conn, error) {
	if path == "" || path == "none" {
		return nil, fmt.Errorf("SSH agent is unavailable")
	}
	return net.DialTimeout("unix", path, agentRelayDialTimeout)
}
