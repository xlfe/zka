package zka

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentRelayClaimsReconnectsAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	originPath := filepath.Join(root, "upstream", "origin.sock")
	firstPath := filepath.Join(root, "upstream", "first.sock")
	secondPath := filepath.Join(root, "upstream", "second.sock")
	newestPath := filepath.Join(root, "upstream", "newest.sock")
	listenTestAgent(t, originPath, "origin")
	listenTestAgent(t, firstPath, "first")
	listenTestAgent(t, secondPath, "second")
	listenTestAgent(t, newestPath, "newest")

	manager := newAgentRelayManager(filepath.Join(root, "relays"), originPath)
	t.Cleanup(manager.close)
	workspace := "0123456789abcdef0123456789abcdef"
	relayPath, err := manager.ensure(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := relayRoundTrip(t, relayPath); got != "origin" {
		t.Fatalf("origin relay = %q", got)
	}
	info, err := os.Stat(relayPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("relay mode = %v, err = %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(relayPath))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("relay dir mode = %v, err = %v", dirInfo, err)
	}

	now := time.Now().UTC()
	manager.register(workspace, agentSource{Attachment: "remote", Pane: "pane-a", Socket: firstPath, Heartbeat: now.Add(-time.Second)}, true)
	manager.register(workspace, agentSource{Attachment: "remote", Pane: "pane-b", Socket: secondPath, Heartbeat: now}, true)
	if !manager.sourceAvailable(workspace, "remote") {
		t.Fatal("fresh forwarded source was unavailable")
	}
	manager.setClaim(workspace, "remote")
	if got := relayRoundTrip(t, relayPath); got != "second" {
		t.Fatalf("newest source = %q", got)
	}

	// A newer heartbeat on another pane does not churn a healthy selected
	// source. Replacing the selected pane's SSH socket does switch immediately.
	manager.register(workspace, agentSource{Attachment: "remote", Pane: "pane-a", Socket: firstPath, Heartbeat: time.Now().UTC()}, true)
	if got := relayRoundTrip(t, relayPath); got != "second" {
		t.Fatalf("sticky source = %q", got)
	}
	manager.register(workspace, agentSource{Attachment: "remote", Pane: "pane-b", Socket: newestPath, Heartbeat: time.Now().UTC()}, true)
	if got := relayRoundTrip(t, relayPath); got != "newest" {
		t.Fatalf("reconnected source = %q", got)
	}

	manager.setClaim(workspace, "missing")
	assertRelayUnavailable(t, relayPath)
	manager.setClaim(workspace, "")
	if got := relayRoundTrip(t, relayPath); got != "origin" {
		t.Fatalf("released relay = %q", got)
	}

	manager.register(workspace, agentSource{Attachment: "expired", Pane: "pane", Socket: firstPath, Heartbeat: now.Add(-7 * time.Second)}, true)
	if manager.sourceAvailable(workspace, "expired") {
		t.Fatal("expired source remained available")
	}
	manager.close()
	if _, err := os.Stat(relayPath); !os.IsNotExist(err) {
		t.Fatalf("relay socket remains after close: %v", err)
	}
}

func TestAgentRelayRefusesNonSocketCollision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := "0123456789abcdef0123456789abcdef"
	path := agentRelaySocketPath(dir, workspace)
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := newAgentRelayManager(dir, "")
	t.Cleanup(manager.close)
	if _, err := manager.ensure(workspace, ""); err == nil {
		t.Fatal("non-socket collision was replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("collision changed: %q, %v", data, err)
	}
}

func TestAgentRelayClosesActiveConnectionWhenClaimChanges(t *testing.T) {
	root := t.TempDir()
	originPath := filepath.Join(root, "origin.sock")
	remotePath := filepath.Join(root, "remote.sock")
	listenTestAgent(t, originPath, "origin")
	listenTestAgent(t, remotePath, "remote")
	manager := newAgentRelayManager(filepath.Join(root, "relays"), originPath)
	t.Cleanup(manager.close)
	workspace := "fedcba9876543210fedcba9876543210"
	relayPath, err := manager.ensure(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("unix", relayPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("origin"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	manager.register(workspace, agentSource{Attachment: "remote", Pane: "pane", Socket: remotePath, Heartbeat: time.Now().UTC()}, true)
	manager.setClaim(workspace, "remote")
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active origin connection survived claim change")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("timed out waiting for claim change to close the connection")
	}
}

func TestAgentRelayExpiresActiveForwardedConnection(t *testing.T) {
	root := t.TempDir()
	remotePath := filepath.Join(root, "remote.sock")
	listenTestAgent(t, remotePath, "remote")
	manager := newAgentRelayManager(filepath.Join(root, "relays"), "")
	t.Cleanup(manager.close)
	workspace := "abcdef0123456789abcdef0123456789"
	relayPath, err := manager.ensure(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	manager.register(workspace, agentSource{
		Attachment: "remote", Pane: "pane", Socket: remotePath, Heartbeat: time.Now().UTC().Add(-5 * time.Second),
	}, true)
	manager.setClaim(workspace, "remote")
	client, err := net.Dial("unix", relayPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("remote"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("forwarded connection survived heartbeat expiry")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("timed out waiting for heartbeat expiry to close the connection")
	}
}

func listenTestAgent(t *testing.T, path, response string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buffer := make([]byte, 1)
				for {
					if _, err := io.ReadFull(conn, buffer); err != nil {
						return
					}
					_, _ = io.WriteString(conn, response)
				}
			}()
		}
	}()
}

func relayRoundTrip(t *testing.T, path string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("origin"))
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	return string(buffer[:n])
}

func assertRelayUnavailable(t *testing.T, path string) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("x"))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("relay unexpectedly reached an upstream")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("unavailable relay left the client connection open")
	}
}
