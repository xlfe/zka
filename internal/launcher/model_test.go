package launcher

import (
	"reflect"
	"testing"

	"github.com/xlfe/zka/internal/zka"
)

func TestSplitWorkspacesSeparatesLocalFromRemoteCache(t *testing.T) {
	workspaces := []*zka.Workspace{
		{ID: "local-z", Name: "Zulu", Panes: map[string]*zka.Pane{}},
		{ID: "remote-a", Name: "Remote", RemoteHost: "devbox.example"},
		{ID: "local-a", Name: "alpha", Panes: map[string]*zka.Pane{}},
		{ID: "remote-b", Name: "Other", RemoteHost: "laptop.example"},
		{ID: "deleting", Name: "Gone", DeletionPending: true},
		nil,
	}
	local, hosts := splitWorkspaces(workspaces, "local-node")
	if len(local) != 2 {
		t.Fatalf("local workspace count = %d, want 2", len(local))
	}
	if got := []string{local[0].ID, local[1].ID}; !reflect.DeepEqual(got, []string{"local-a", "local-z"}) {
		t.Fatalf("local workspaces = %#v", got)
	}
	if !reflect.DeepEqual(hosts, []string{"devbox.example", "laptop.example"}) {
		t.Fatalf("remote hosts = %#v", hosts)
	}
}

func TestSplitWorkspacesIncludesWorkspacesPreviouslyConnectedToThisNode(t *testing.T) {
	localNode := "local-node"
	workspaces := []*zka.Workspace{
		{
			ID: "remote-attached", Name: "Remote attached", RemoteHost: "devbox.example",
			Attachments: map[string]*zka.Attachment{"local": {
				Node: zka.Host{ID: localNode}, Endpoint: "unix:/local/attached.sock", Status: zka.AttachmentReady,
			}},
		},
		{
			ID: "remote-detached", Name: "Remote detached", RemoteHost: "laptop.example",
			Attachments: map[string]*zka.Attachment{"local": {
				Node: zka.Host{ID: localNode}, Endpoint: "unix:/local/detached.sock", Status: zka.AttachmentDetached,
			}},
		},
		{ID: "remote-unseen", Name: "Remote unseen", RemoteHost: "other"},
	}
	visible, hosts := splitWorkspaces(workspaces, localNode)
	if got := []string{visible[0].ID, visible[1].ID}; !reflect.DeepEqual(got, []string{"remote-attached", "remote-detached"}) {
		t.Fatalf("visible workspaces = %#v", got)
	}
	if !reflect.DeepEqual(hosts, []string{"devbox.example", "laptop.example", "other"}) {
		t.Fatalf("remote hosts = %#v", hosts)
	}
}

func TestLocalWorkspaceItemsGroupAttachedBeforeDetached(t *testing.T) {
	localNode := "local-node"
	attached := &zka.Workspace{
		ID: "attached", Name: "Attached",
		Attachments: map[string]*zka.Attachment{"local": {
			Node: zka.Host{ID: localNode}, Endpoint: "unix:/attached.sock", Status: zka.AttachmentReady,
		}},
	}
	detached := &zka.Workspace{ID: "detached", Name: "Detached"}
	items := localWorkspaceItems([]*zka.Workspace{attached, detached}, localNode)
	if len(items) != 4 || items[0].label != "ATTACHED" || items[1].workspace != attached ||
		items[2].label != "DETACHED" || items[3].workspace != detached {
		t.Fatalf("items = %#v", items)
	}
	if items[1].selection != 0 || items[3].selection != 1 {
		t.Fatalf("selection indexes = %d, %d", items[1].selection, items[3].selection)
	}
}

func TestLauncherBuildsExistingCLICommandsWithoutAShell(t *testing.T) {
	workspace := &zka.Workspace{ID: "0123456789abcdef", Name: "main"}
	for _, test := range []struct {
		name string
		got  []string
		want []string
	}{
		{name: "automatic name", got: createArgs("  "), want: []string{"kitty"}},
		{name: "explicit name", got: createArgs("  shell work  "), want: []string{"kitty", "--name", "shell work"}},
		{name: "local attach", got: attachArgs("", workspace), want: []string{"workspace", "attach", workspace.ID}},
		{name: "remote attach", got: attachArgs("devbox.example", workspace), want: []string{"workspace", "attach", "devbox.example:" + workspace.ID}},
		{name: "detach", got: detachArgs(workspace), want: []string{"workspace", "detach", workspace.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("args = %#v, want %#v", test.got, test.want)
			}
		})
	}
}

func TestWorkspaceSummaryIncludesAggregateStateAgentsTopologyAndShortID(t *testing.T) {
	workspace := &zka.Workspace{
		ID:        "0123456789abcdef",
		Attention: zka.StateWorking,
		Panes: map[string]*zka.Pane{
			"one":   {Agent: "codex"},
			"two":   {Agent: "codex"},
			"three": {},
		},
		Manifest: zka.Manifest{Topology: []zka.Node{{Kind: "os-window", Children: []zka.Node{
			{Kind: "tab", Children: []zka.Node{{Kind: "pane"}, {Kind: "pane"}}},
			{Kind: "tab", Children: []zka.Node{{Kind: "pane"}}},
		}}}},
	}
	if got, want := workspaceSummary(workspace), "Working  ·  agents: codex ×2  ·  3 panes / 2 tabs / 1 window  ·  01234567"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	workspace.Attention = zka.StateBlocked
	workspace.RemoteHost = "devbox.example"
	if got, want := workspaceSummary(workspace), "devbox.example  ·  Waiting for you  ·  agents: codex ×2  ·  3 panes / 2 tabs / 1 window  ·  01234567"; got != want {
		t.Fatalf("remote summary = %q, want %q", got, want)
	}
}
