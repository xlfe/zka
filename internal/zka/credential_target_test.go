package zka

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialTargetCachesPublishedOpenPGPSocketPathPerGeneration(t *testing.T) {
	root := testRoot(t)
	paths := testPaths(root)
	socketDir := filepath.Join(root, "gpg-sockets")
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "S.gpg-agent")
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		switch args[len(args)-1] {
		case "socketdir":
			return socketDir + "\n", "", nil
		case "agent-socket":
			return socketPath + "\n", "", nil
		default:
			return "", "", nil
		}
	}}
	session := &credentialTargetSession{
		paths: paths, runner: runner, socketPaths: map[string]string{},
		config: defaultConfig(),
	}
	first, err := session.cachedOpenPGPSocketPath(context.Background(), "workspace", 1)
	if err != nil || first != socketPath {
		t.Fatalf("first path = %q, %v", first, err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	second, err := session.cachedOpenPGPSocketPath(context.Background(), "workspace", 1)
	if err != nil || second != socketPath {
		t.Fatalf("cached path = %q, %v", second, err)
	}
	if calls := runner.Calls(); len(calls) != 2 {
		t.Fatalf("published socket re-ran gpgconf: %#v", calls)
	}
	if _, err := session.cachedOpenPGPSocketPath(context.Background(), "workspace", 2); err != nil {
		t.Fatal(err)
	}
	if calls := runner.Calls(); len(calls) != 4 || !strings.Contains(strings.Join(calls[3].Args, " "), "agent-socket") {
		t.Fatalf("new generation did not re-resolve gpgconf: %#v", calls)
	}
}
