package launcher

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"gioui.org/app"
	"gioui.org/layout"

	"github.com/xlfe/zka/internal/zka"
)

type recordingLauncherBackend struct {
	unavailableBackend
	executed   chan []string
	executeErr error
}

func (b *recordingLauncherBackend) Execute(_ context.Context, args []string) error {
	b.executed <- append([]string(nil), args...)
	return b.executeErr
}

func TestForgetDetachedRemoteWorkspaceConfirmationFlow(t *testing.T) {
	workspace := &zka.Workspace{ID: "0123456789abcdef", Name: "api", RemoteHost: "devbox.example"}
	backend := &recordingLauncherBackend{executed: make(chan []string, 1)}
	ui := newUI(backend)
	ui.ctx = context.Background()
	ui.window = new(app.Window)
	ui.screen = screenHome
	ui.local = []*zka.Workspace{workspace}
	ui.selected = 2

	ui.forgetSelection()
	if ui.screen != screenForget || ui.forgetWorkspace != workspace {
		t.Fatalf("forget action landed on screen %d with workspace %#v", ui.screen, ui.forgetWorkspace)
	}
	ui.back()
	if ui.screen != screenHome || ui.forgetWorkspace != nil || ui.selected != 2 {
		t.Fatalf("cancel returned screen=%d workspace=%#v selected=%d", ui.screen, ui.forgetWorkspace, ui.selected)
	}

	ui.forgetSelection()
	ui.confirmForget()
	if !ui.busy || ui.operationKind != resultForget {
		t.Fatalf("confirm busy=%t operation=%d", ui.busy, ui.operationKind)
	}
	ui.back()
	if ui.screen != screenForget {
		t.Fatalf("back left the confirmation while forget was running: screen=%d", ui.screen)
	}

	select {
	case got := <-backend.executed:
		want := []string{"workspace", "forget", "devbox.example:0123456789abcdef"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("forget args = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("forget command did not execute")
	}
	select {
	case result := <-ui.results:
		ui.results <- result
	case <-time.After(time.Second):
		t.Fatal("forget command did not return a result")
	}
	ui.drainResults()
	if ui.screen != screenHome || ui.forgetWorkspace != nil || ui.busy {
		t.Fatalf("success left screen=%d workspace=%#v busy=%t", ui.screen, ui.forgetWorkspace, ui.busy)
	}
}

func TestForgetDetachedRemoteWorkspaceFailureStaysOnConfirmation(t *testing.T) {
	workspace := &zka.Workspace{ID: "0123456789abcdef", Name: "api", RemoteHost: "devbox.example"}
	backend := &recordingLauncherBackend{
		executed:   make(chan []string, 1),
		executeErr: errors.New("workspace is still attached"),
	}
	ui := newUI(backend)
	ui.ctx = context.Background()
	ui.window = new(app.Window)
	ui.screen = screenHome
	ui.local = []*zka.Workspace{workspace}
	ui.selected = 2
	ui.forgetSelection()
	ui.confirmForget()

	select {
	case <-backend.executed:
	case <-time.After(time.Second):
		t.Fatal("forget command did not execute")
	}
	select {
	case result := <-ui.results:
		ui.results <- result
	case <-time.After(time.Second):
		t.Fatal("forget command did not return a result")
	}
	ui.drainResults()
	if ui.screen != screenForget || ui.forgetWorkspace != workspace || ui.busy {
		t.Fatalf("failure left screen=%d workspace=%#v busy=%t", ui.screen, ui.forgetWorkspace, ui.busy)
	}
	if ui.errorMessage != "workspace is still attached" {
		t.Fatalf("error = %q", ui.errorMessage)
	}
}

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

func TestRemoteAttachAndCreateCanClaimCredentialBundle(t *testing.T) {
	workspace := &zka.Workspace{ID: "aaaa", Name: "api"}
	if got, want := remoteAttachArgs("devbox.example", workspace, true, "work"),
		[]string{"workspace", "attach", "devbox.example:aaaa", "--claim-credentials", "--credential-bundle", "work"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote attach args = %#v, want %#v", got, want)
	}
	if got, want := remoteCreateArgs("devbox.example", " api ", true, "work"),
		[]string{"workspace", "create", "devbox.example:api", "--attach", "--credential-bundle", "work"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote create args = %#v, want %#v", got, want)
	}
	if got, want := remoteAttachArgs("devbox.example", workspace, false, ""),
		[]string{"workspace", "attach", "devbox.example:aaaa"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain remote attach args = %#v, want %#v", got, want)
	}
	if got, want := remoteCreateArgs("devbox.example", "api", false, "work"),
		[]string{"workspace", "create", "devbox.example:api", "--attach", "--no-credentials"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote create opt-out args = %#v, want %#v", got, want)
	}
}

func TestRemoteListCredentialShortcutTogglesExplicitClaimChoice(t *testing.T) {
	ui := newUI(nil)
	ui.screen = screenRemoteList
	ui.credentialsEnabled = true
	ui.defaultBundle = "work"

	ui.toggleCredentialSelection()
	if !ui.remoteCredentials.Value {
		t.Fatal("credential shortcut did not enable the remote claim choice")
	}
	ui.toggleCredentialSelection()
	if ui.remoteCredentials.Value {
		t.Fatal("credential shortcut did not disable the remote claim choice")
	}
	ui.defaultBundle = ""
	ui.toggleCredentialSelection()
	if ui.remoteCredentials.Value {
		t.Fatal("credential shortcut enabled without a default bundle")
	}
}

func TestRemoteWorkspaceCredentialOwnershipAndActions(t *testing.T) {
	workspace := &zka.Workspace{
		ID: "0123456789abcdef", Name: "api", RemoteHost: "devbox.example",
		Attachments: map[string]*zka.Attachment{
			"local": {ID: "local", Node: zka.Host{ID: "local-node", Name: "laptop"}},
			"other": {ID: "other", Node: zka.Host{ID: "other-node", Name: "desktop"}},
		},
	}
	if got, want := workspaceCredentialSummary(workspace, workspace.RemoteHost, "local-node"), "Credentials: unclaimed"; got != want {
		t.Fatalf("origin summary = %q, want %q", got, want)
	}
	workspace.CredentialClaim = &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "local-node", Capabilities: map[string]zka.CredentialCapabilityStatus{"ssh-agent": {State: "ready", Available: true}}}
	if got, want := workspaceCredentialSummary(workspace, workspace.RemoteHost, "local-node"), "Credentials: work (ssh-agent ready) · this machine"; got != want {
		t.Fatalf("local summary = %q, want %q", got, want)
	}
	if args, status := workspaceCredentialAction(workspace, "local-node"); !reflect.DeepEqual(args,
		[]string{"workspace", "credentials", "release", "devbox.example:0123456789abcdef"}) || status != "Releasing credentials from" {
		t.Fatalf("release action = %#v, %q", args, status)
	}
	workspace.CredentialClaim.OwnerNodeID = "other-node"
	if got, want := workspaceCredentialSummary(workspace, workspace.RemoteHost, "local-node"), "Credentials: work (ssh-agent ready) · desktop"; got != want {
		t.Fatalf("other summary = %q, want %q", got, want)
	}
	if args, status := workspaceCredentialAction(workspace, "local-node"); !reflect.DeepEqual(args,
		[]string{"workspace", "credentials", "claim", "devbox.example:0123456789abcdef"}) || status != "Claiming credentials for" {
		t.Fatalf("claim action = %#v, %q", args, status)
	}
}

func TestDetachedRemoteWorkspaceCanAttachAndClaimCredentialsInOneAction(t *testing.T) {
	const localNode = "local-node"
	workspace := &zka.Workspace{
		ID: "0123456789abcdef", Name: "api", RemoteHost: "devbox.example",
		CredentialClaim: &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "other-node"},
		Attachments: map[string]*zka.Attachment{
			"local": {
				ID: "local", Node: zka.Host{ID: localNode, Name: "laptop"},
				Endpoint: "unix:/local/detached.sock", Status: zka.AttachmentDetached,
			},
			"other": {ID: "other", Node: zka.Host{ID: "other-node", Name: "desktop"}},
		},
	}
	backend := &recordingLauncherBackend{executed: make(chan []string, 1)}
	ui := newUI(backend)
	ui.ctx = context.Background()
	ui.window = new(app.Window)
	ui.screen = screenHome
	ui.localNodeID = localNode
	ui.local = []*zka.Workspace{workspace}
	ui.selected = 2
	ui.credentialsEnabled = true
	ui.defaultBundle = "work"

	if !ui.workspaceCredentialControlVisible(workspace) {
		t.Fatal("detached remote workspace did not offer a credential action")
	}
	if got, want := workspaceCredentialButtonLabel(workspace, localNode), "Attach + claim credentials"; got != want {
		t.Fatalf("credential button label = %q, want %q", got, want)
	}

	ui.toggleCredentialSelection()

	select {
	case got := <-backend.executed:
		want := []string{"workspace", "attach", "devbox.example:0123456789abcdef", "--claim-credentials", "--credential-bundle", "work"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("credential action args = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("detached workspace credential action did not execute")
	}
}

func TestOriginWorkspaceCanAttachAndReleaseCredentialsInOneAction(t *testing.T) {
	const originNode = "origin-node"
	workspace := &zka.Workspace{
		ID: "0123456789abcdef", Name: "api",
		CredentialClaim: &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "machine-a-node", Capabilities: map[string]zka.CredentialCapabilityStatus{"openpgp": {State: "ready", Available: true}}},
		Attachments: map[string]*zka.Attachment{
			"origin": {
				ID: "origin", Node: zka.Host{ID: originNode, Name: "devbox"},
				Endpoint: "unix:/devbox/detached.sock", Status: zka.AttachmentDetached,
			},
			"machine-a": {ID: "machine-a", Node: zka.Host{ID: "machine-a-node", Name: "machine-a"}},
		},
	}
	backend := &recordingLauncherBackend{executed: make(chan []string, 2)}
	ui := newUI(backend)
	ui.ctx = context.Background()
	ui.window = new(app.Window)
	ui.screen = screenHome
	ui.localNodeID = originNode
	ui.local = []*zka.Workspace{workspace}
	ui.selected = 2
	ui.credentialsEnabled = true
	ui.defaultBundle = "work"

	if got, want := workspaceCredentialSummary(workspace, "", originNode), "Credentials: work (openpgp ready) · machine-a"; got != want {
		t.Fatalf("origin summary = %q, want %q", got, want)
	}
	if !ui.workspaceCredentialControlVisible(workspace) {
		t.Fatal("origin workspace did not offer a credential action for a remote claim")
	}
	if got, want := workspaceCredentialButtonLabel(workspace, originNode), "Attach + release credentials"; got != want {
		t.Fatalf("origin credential button label = %q, want %q", got, want)
	}

	ui.toggleCredentialSelection()

	for index, want := range [][]string{
		{"workspace", "credentials", "release", "0123456789abcdef"},
		{"workspace", "attach", "0123456789abcdef"},
	} {
		select {
		case got := <-backend.executed:
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("origin action %d args = %#v, want %#v", index, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("origin action %d did not execute", index)
		}
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
