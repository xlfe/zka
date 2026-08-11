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
	// Version 5 adds cooperative PIVB route-required projection.
	legacyCredentialEnvironmentVersion = 2
	credentialEnvironmentVersion       = 5
	agentRelayDialTimeout              = 500 * time.Millisecond
)

const pivbRoutingEnvironment = "environment"

func credentialEnvironmentVersionForConfig(Config) int {
	return credentialEnvironmentVersion
}

func configHasPIVBBundle(cfg Config) bool {
	for _, bundle := range cfg.Credentials.Bundles {
		if bundle.PIVB.Enable {
			return true
		}
	}
	return false
}

func managedPIVBAttachmentProtocol(Config) int {
	return 1
}

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
