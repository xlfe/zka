package zka

import (
	"context"
	"encoding/json"
	"net"
	"strings"
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
