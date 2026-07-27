package launcher

import (
	"image"
	"testing"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

func TestLauncherMessageIsSelectableAndExposesSelectedText(t *testing.T) {
	ui := newUI(nil)
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Constraints: layout.Exact(image.Pt(640, 120)),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	const message = "kitty views are not ready: attachment is missing a ready pane"
	ui.message(gtx, "operation-error", message, true)

	first := ui.selectable("operation-error")
	first.SetCaret(0, len([]rune(message)))

	if got := first.SelectedText(); got != message {
		t.Fatalf("selected text = %q, want %q", got, message)
	}
	if second := ui.selectable("operation-error"); second != first {
		t.Fatal("selectable state was replaced between frames")
	}
	if other := ui.selectable("workspace-summary"); other == first {
		t.Fatal("unrelated labels shared selection state")
	}
}
