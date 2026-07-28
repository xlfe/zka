package launcher

import (
	"reflect"
	"testing"

	"gioui.org/layout"

	"github.com/xlfe/zka/internal/zka"
)

func TestRemoteListSelectionIncludesCreateRow(t *testing.T) {
	ui := newUI(nil)
	ui.screen = screenRemoteList
	ui.remoteHost = "devbox.example"
	ui.remote = sortRemoteWorkspaces([]*zka.Workspace{{ID: "aaaa", Name: "a"}, {ID: "bbbb", Name: "b"}})
	if got := ui.selectionCount(); got != 3 {
		t.Fatalf("selection count = %d, want new row + 2 workspaces", got)
	}
	ui.openRemoteCreate()
	if ui.screen != screenRemoteCreate {
		t.Fatalf("openRemoteCreate landed on screen %d", ui.screen)
	}
	if ui.focusPending != &ui.remoteNameEditor {
		t.Fatal("remote create did not focus its name editor")
	}
	if got := ui.selectionCount(); got != 0 {
		t.Fatalf("remote create selection count = %d", got)
	}
	ui.back()
	if ui.screen != screenRemoteList {
		t.Fatalf("back from remote create landed on screen %d", ui.screen)
	}
}

func TestRemoteAttachAndCreateCanClaimThisMachinesAgent(t *testing.T) {
	workspace := &zka.Workspace{ID: "aaaa", Name: "api"}
	if got, want := remoteAttachArgs("devbox.example", workspace, true),
		[]string{"workspace", "attach", "devbox.example:aaaa", "--claim-agent"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote attach args = %#v, want %#v", got, want)
	}
	if got, want := remoteCreateArgs("devbox.example", " api ", true),
		[]string{"workspace", "create", "devbox.example:api", "--attach", "--claim-agent"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote create args = %#v, want %#v", got, want)
	}
	if got, want := remoteAttachArgs("devbox.example", workspace, false),
		[]string{"workspace", "attach", "devbox.example:aaaa"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain remote attach args = %#v, want %#v", got, want)
	}
}

func TestRemoteListAgentShortcutTogglesExplicitClaimChoice(t *testing.T) {
	ui := newUI(nil)
	ui.screen = screenRemoteList
	ui.agentForwarding = true

	ui.toggleAgentSelection()
	if !ui.remoteAgent.Value {
		t.Fatal("agent shortcut did not enable the remote claim choice")
	}
	ui.toggleAgentSelection()
	if ui.remoteAgent.Value {
		t.Fatal("agent shortcut did not disable the remote claim choice")
	}
	ui.agentForwarding = false
	ui.toggleAgentSelection()
	if ui.remoteAgent.Value {
		t.Fatal("agent shortcut enabled a locally disabled forwarding configuration")
	}
}

func TestRemoteWorkspaceAgentOwnershipAndActions(t *testing.T) {
	workspace := &zka.Workspace{
		ID: "0123456789abcdef", Name: "api", RemoteHost: "devbox.example",
		Attachments: map[string]*zka.Attachment{
			"local": {ID: "local", Node: zka.Host{ID: "local-node", Name: "laptop"}},
			"other": {ID: "other", Node: zka.Host{ID: "other-node", Name: "desktop"}},
		},
	}
	if got, want := workspaceSSHAgentSummary(workspace, workspace.RemoteHost, "local-node"), "SSH agent: origin"; got != want {
		t.Fatalf("origin summary = %q, want %q", got, want)
	}
	workspace.AgentAttachmentID = "local"
	if got, want := workspaceSSHAgentSummary(workspace, workspace.RemoteHost, "local-node"), "SSH agent: this machine"; got != want {
		t.Fatalf("local summary = %q, want %q", got, want)
	}
	if args, status := workspaceAgentAction(workspace, "local-node"); !reflect.DeepEqual(args,
		[]string{"workspace", "agent", "release", "devbox.example:0123456789abcdef"}) || status != "Releasing the SSH agent from" {
		t.Fatalf("release action = %#v, %q", args, status)
	}
	workspace.AgentAttachmentID = "other"
	if got, want := workspaceSSHAgentSummary(workspace, workspace.RemoteHost, "local-node"), "SSH agent: desktop"; got != want {
		t.Fatalf("other summary = %q, want %q", got, want)
	}
	if args, status := workspaceAgentAction(workspace, "local-node"); !reflect.DeepEqual(args,
		[]string{"workspace", "agent", "claim", "devbox.example:0123456789abcdef"}) || status != "Using this machine's SSH agent for" {
		t.Fatalf("claim action = %#v, %q", args, status)
	}
}

func TestKeyboardSelectionScrollsRemoteListToNewlySelectedWorkspace(t *testing.T) {
	ui := newUI(nil)
	ui.screen = screenRemoteList
	ui.remote = []*zka.Workspace{
		{ID: "one"}, {ID: "two"}, {ID: "three"}, {ID: "four"},
	}
	ui.selected = 2
	ui.remoteList.Position = layout.Position{First: 0, Count: 2}

	ui.moveSelection(1)

	if ui.selected != 3 {
		t.Fatalf("selected = %d, want 3", ui.selected)
	}
	if got, want := ui.remoteList.Position.First, 2; got != want {
		t.Fatalf("first remote list item = %d, want selected item %d", got, want)
	}
}

func TestKeyboardSelectionScrollsGroupedHomeListToNewlySelectedWorkspace(t *testing.T) {
	ui := newUI(nil)
	ui.screen = screenHome
	ui.local = []*zka.Workspace{
		{ID: "one"}, {ID: "two"}, {ID: "three"}, {ID: "four"},
	}
	// Selections 0 and 1 are the fixed create rows. The list itself starts
	// with a DETACHED section header, so selection 3 maps to list item 2.
	ui.selected = 3
	ui.localList.Position = layout.Position{First: 0, Count: 3}

	ui.moveSelection(1)

	if ui.selected != 4 {
		t.Fatalf("selected = %d, want 4", ui.selected)
	}
	if got, want := ui.localList.Position.First, 3; got != want {
		t.Fatalf("first local list item = %d, want selected grouped item %d", got, want)
	}
}
