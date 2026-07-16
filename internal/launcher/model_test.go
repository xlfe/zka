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
	local, hosts := splitWorkspaces(workspaces)
	if len(local) != 2 {
		t.Fatalf("local workspace count = %d, want 2", len(local))
	}
	if got := []string{local[0].ID, local[1].ID}; !reflect.DeepEqual(got, []string{"local-a", "local-z"}) {
		t.Fatalf("local workspaces = %#v", got)
	}
	if !reflect.DeepEqual(hosts, []string{"laptop.example", "devbox.example"}) {
		t.Fatalf("remote hosts = %#v", hosts)
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
	} {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("args = %#v, want %#v", test.got, test.want)
			}
		})
	}
}

func TestWorkspaceSummaryIncludesStatePaneCountAndShortID(t *testing.T) {
	workspace := &zka.Workspace{
		ID:        "0123456789abcdef",
		Attention: zka.StateWorking,
		Panes: map[string]*zka.Pane{
			"one": {},
			"two": {},
		},
	}
	if got, want := workspaceSummary(workspace), "working  ·  2 panes  ·  01234567"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	delete(workspace.Panes, "two")
	if got, want := workspaceSummary(workspace), "working  ·  1 pane  ·  01234567"; got != want {
		t.Fatalf("singular summary = %q, want %q", got, want)
	}
}
