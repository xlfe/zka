package zka

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatcherBurstDebouncesToOneAuthoritativeCapture(t *testing.T) {
	var workspaceID, paneID string
	runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		if name != "kitten" {
			return "", "", nil
		}
		if joined == "--version" {
			return "kitten 0.47.4\n", "", nil
		}
		if strings.Contains(joined, "--output-format=session") {
			return "new_tab work\nlayout splits\nlaunch --var zka_workspace=" + workspaceID + " --var zka_pane=" + paneID + " fish\n", "", nil
		}
		if strings.Contains(joined, " ls") {
			return `[{"id":1,"tabs":[{"id":2,"title":"work","layout":"splits","windows":[{"id":3,"title":"shell","cwd":"/work","user_vars":{"zka_workspace":"` + workspaceID + `","zka_pane":"` + paneID + `","zka_ready":"1"}}]}]}]`, "", nil
		}
		return "", "", nil
	}}
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	workspaceID, paneID = workspace.ID, firstPane(workspace).ID
	if _, err := d.registerAttachment(workspace.ID, Attachment{ID: "local", Node: d.state.Node, Transport: Transport{Kind: "local"}, Endpoint: "unix:/kitty"}); err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	address := &net.UnixAddr{Name: d.paths.WatcherSocket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 8; i++ {
		payload, _ := json.Marshal(WatcherEvent{Version: 1, Endpoint: "unix:/kitty", Workspace: workspace.ID, Kind: "resize", Timestamp: time.Now().UTC()})
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		got, getErr := d.getWorkspace(workspace.ID)
		return getErr == nil && got.Manifest.Session != ""
	})
	time.Sleep(100 * time.Millisecond)
	listCalls := 0
	for _, call := range runner.Calls() {
		if call.Name == "kitten" && strings.Contains(strings.Join(call.Args, " "), " ls") {
			listCalls++
		}
	}
	if listCalls != 2 {
		t.Fatalf("Kitty ls calls = %d, want one JSON and one session capture", listCalls)
	}
}

func TestWatcherCloseRemovesPaneAndKillsItsZMXSession(t *testing.T) {
	var mu sync.Mutex
	var workspaceID, closedPaneID, remainingPaneID string
	sessions := map[string]bool{}
	runner := &fakeRunner{handler: func(_ context.Context, name string, args ...string) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		joined := strings.Join(args, " ")
		if name == "kitten" {
			if joined == "--version" {
				return "kitten 0.47.4\n", "", nil
			}
			if strings.Contains(joined, "--output-format=session") {
				return fmt.Sprintf("launch --var zka_workspace=%s --var zka_pane=%s fish\n", workspaceID, remainingPaneID), "", nil
			}
			if strings.Contains(joined, " ls") {
				return fmt.Sprintf(`[{"id":1,"tabs":[{"id":2,"layout":"splits","windows":[{"id":3,"title":"shell","cwd":"/work","user_vars":{"zka_workspace":"%s","zka_pane":"%s","zka_ready":"1"}}]}]}]`, workspaceID, remainingPaneID), "", nil
			}
			return "", "", nil
		}
		if len(args) >= 2 && args[0] == "list" {
			var active []string
			for session := range sessions {
				active = append(active, session)
			}
			return strings.Join(active, "\n") + "\n", "", nil
		}
		if len(args) >= 2 && args[0] == "kill" {
			delete(sessions, args[1])
			return "", "", nil
		}
		return "", "", nil
	}}
	d, err := newTestDaemon(t, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	panes := workspace.SortedPanes()
	workspaceID, closedPaneID, remainingPaneID = workspace.ID, panes[0].ID, panes[1].ID
	mu.Lock()
	sessions[panes[0].Backend.Ref] = true
	sessions[panes[1].Backend.Ref] = true
	mu.Unlock()
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	serveTestDaemon(t, d)
	d.events <- WatcherEvent{Version: 1, Endpoint: attachment.Endpoint, Workspace: workspace.ID, Kind: "close", PaneID: closedPaneID}
	waitFor(t, func() bool {
		got, getErr := d.getWorkspace(workspace.ID)
		return getErr == nil && got.Panes[closedPaneID] == nil && got.Panes[remainingPaneID] != nil
	})
	mu.Lock()
	closedAlive, remainingAlive := sessions[panes[0].Backend.Ref], sessions[panes[1].Backend.Ref]
	mu.Unlock()
	if closedAlive || !remainingAlive {
		t.Fatalf("sessions after close: closed=%v remaining=%v", closedAlive, remainingAlive)
	}
}
