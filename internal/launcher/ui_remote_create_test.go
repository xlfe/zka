package launcher

import (
	"testing"

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
