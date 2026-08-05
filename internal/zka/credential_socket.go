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
	// Pane environment version 2 is the first version that receives both the
	// stable SSH endpoint and the per-workspace GNUPGHOME when configured.
	credentialEnvironmentVersion = 2
	agentRelayDialTimeout        = 500 * time.Millisecond
)

// agentRelaySocketPath is stable across claim generations and transport
// reconnects, so a pane's SSH_AUTH_SOCK never depends on a particular SSH
// control process.
func agentRelaySocketPath(dir, workspaceID string) string {
	const safeUnixSocketPath = 103
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
