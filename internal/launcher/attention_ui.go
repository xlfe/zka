package launcher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/xlfe/zka/internal/zka"
)

type attentionResult struct {
	snapshot *zka.AttentionSnapshot
	err      error
	action   bool
	command  bool
}

type attentionUI struct {
	base    *ui
	backend Backend
	window  *app.Window
	ctx     context.Context
	cancel  context.CancelFunc
	results chan attentionResult

	snapshot     zka.AttentionSnapshot
	loaded       bool
	unavailable  string
	selected     int
	busy         bool
	status       string
	errorMessage string

	rows        map[string]*widget.Clickable
	pauseButton widget.Clickable
	list        widget.List
}

func newAttentionUI(backend Backend) *attentionUI {
	application := &attentionUI{
		base:    newUI(backend),
		backend: backend,
		results: make(chan attentionResult, 16),
		rows:    map[string]*widget.Clickable{},
	}
	application.list.Axis = layout.Vertical
	return application
}

func (ui *attentionUI) run(w *app.Window) error {
	ui.window = w
	ui.base.window = w
	ui.ctx, ui.cancel = context.WithCancel(context.Background())
	ui.base.ctx = ui.ctx
	ui.base.cancel = ui.cancel
	defer ui.cancel()
	go ui.watch()
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.drainResults()
			area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
			event.Op(gtx.Ops, w)
			ui.handleKeys(gtx)
			ui.handleClicks(gtx)
			ui.layout(gtx)
			area.Pop()
			e.Frame(gtx.Ops)
		}
	}
}

func (ui *attentionUI) watch() {
	backoff := 250 * time.Millisecond
	for ui.ctx.Err() == nil {
		received := false
		err := ui.backend.WatchAttention(ui.ctx, func(snapshot zka.AttentionSnapshot) error {
			received = true
			backoff = 250 * time.Millisecond
			copy := snapshot
			ui.deliver(attentionResult{snapshot: &copy})
			return nil
		})
		if ui.ctx.Err() != nil {
			return
		}
		ui.deliver(attentionResult{err: err})
		if received {
			backoff = 250 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ui.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

func (ui *attentionUI) deliver(result attentionResult) {
	select {
	case ui.results <- result:
	case <-ui.ctx.Done():
		return
	}
	ui.window.Invalidate()
}

func (ui *attentionUI) drainResults() {
	for {
		select {
		case result := <-ui.results:
			if result.action {
				ui.busy = false
				ui.status = ""
				if result.err != nil {
					ui.errorMessage = result.err.Error()
					continue
				}
				ui.window.Perform(system.ActionClose)
				continue
			}
			if result.command {
				ui.busy = false
				ui.status = ""
				if result.err != nil {
					ui.errorMessage = result.err.Error()
				}
				continue
			}
			if result.snapshot != nil {
				selectedID := ""
				if ui.selected >= 0 && ui.selected < len(ui.snapshot.Items) {
					selectedID = ui.snapshot.Items[ui.selected].ID
				}
				ui.snapshot = *result.snapshot
				ui.loaded = true
				ui.unavailable = ""
				if selectedID != "" {
					for index, item := range ui.snapshot.Items {
						if item.ID == selectedID {
							ui.selected = index
							break
						}
					}
				}
				ui.clampSelection()
				continue
			}
			if result.err != nil {
				ui.unavailable = result.err.Error()
			}
		default:
			return
		}
	}
}

func (ui *attentionUI) row(id string) *widget.Clickable {
	button := ui.rows[id]
	if button == nil {
		button = new(widget.Clickable)
		ui.rows[id] = button
	}
	return button
}

func (ui *attentionUI) handleKeys(gtx layout.Context) {
	filters := []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: key.NameUpArrow},
		key.Filter{Name: key.NameDownArrow},
		key.Filter{Name: key.NameReturn},
		key.Filter{Name: key.NameEnter},
		key.Filter{Name: "P"},
		key.Filter{Name: "p"},
	}
	for {
		raw, ok := gtx.Event(filters...)
		if !ok {
			return
		}
		pressed, ok := raw.(key.Event)
		if !ok || pressed.State != key.Press {
			continue
		}
		switch pressed.Name {
		case key.NameEscape:
			ui.window.Perform(system.ActionClose)
		case key.NameUpArrow:
			if ui.selected > 0 {
				ui.selected--
			}
		case key.NameDownArrow:
			if ui.selected+1 < len(ui.snapshot.Items) {
				ui.selected++
			}
		case key.NameReturn, key.NameEnter:
			ui.activateSelection()
		case "P", "p":
			ui.togglePause()
		}
	}
}

func (ui *attentionUI) handleClicks(gtx layout.Context) {
	if ui.pauseButton.Clicked(gtx) {
		ui.togglePause()
		return
	}
	for index, item := range ui.snapshot.Items {
		if ui.row(item.ID).Clicked(gtx) {
			ui.selected = index
			ui.activateSelection()
			return
		}
	}
}

func (ui *attentionUI) activateSelection() {
	if ui.busy || ui.selected < 0 || ui.selected >= len(ui.snapshot.Items) {
		return
	}
	item := ui.snapshot.Items[ui.selected]
	ui.busy = true
	ui.status = "Opening " + item.WorkspaceName + "…"
	ui.errorMessage = ""
	go func() {
		ctx, cancel := context.WithTimeout(ui.ctx, 60*time.Second)
		defer cancel()
		err := ui.backend.Execute(ctx, attentionActionArgs(item))
		ui.deliver(attentionResult{action: true, err: err})
	}()
}

func attentionActionArgs(item zka.AttentionItem) []string {
	if item.Attached {
		// Remote workspaces attached on this node live in the local cache, so
		// focus the local workspace ID instead of asking the origin to attach.
		return []string{"workspace", "focus", item.WorkspaceID, "--pane", item.PaneID}
	}
	return []string{"workspace", "attach", item.WorkspaceRef(), "--pane", item.PaneID}
}

func (ui *attentionUI) togglePause() {
	if ui.busy {
		return
	}
	ui.busy = true
	ui.errorMessage = ""
	ui.status = "Updating attention mode…"
	go func() {
		ctx, cancel := context.WithTimeout(ui.ctx, 10*time.Second)
		defer cancel()
		err := ui.backend.Execute(ctx, []string{"attention", "toggle"})
		// The mode update itself arrives over the live stream; this result only
		// clears command progress or exposes an execution error.
		ui.deliver(attentionResult{command: true, err: err})
	}()
}

func (ui *attentionUI) clampSelection() {
	if len(ui.snapshot.Items) == 0 {
		ui.selected = 0
	} else if ui.selected >= len(ui.snapshot.Items) {
		ui.selected = len(ui.snapshot.Items) - 1
	}
}

func (ui *attentionUI) layout(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, ui.base.colors.background, clip.Rect{Max: gtx.Constraints.Max}.Op())
	gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(time.Minute)})
	return layout.Inset{Top: 28, Right: 30, Bottom: 20, Left: 30}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						brand := material.H6(ui.base.theme, "zka")
						brand.Color = ui.base.colors.accent
						return brand.Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{} }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Pause notifications"
						if ui.snapshot.Paused {
							label = "Resume notifications"
						}
						return ui.base.secondaryButton(gtx, &ui.pauseButton, label)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					heading := material.H5(ui.base.theme, attentionHeading(ui.snapshot))
					return heading.Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 5, Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					label := material.Body2(ui.base.theme, attentionSubtitle(ui.snapshot))
					label.Color = ui.base.colors.muted
					return label.Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				message := ui.errorMessage
				danger := message != ""
				if message == "" && ui.unavailable != "" {
					message = "Live updates unavailable; reconnecting: " + ui.unavailable
					danger = true
				}
				if message == "" && ui.status != "" {
					message = ui.status
				}
				if message == "" {
					return layout.Dimensions{}
				}
				return layout.Inset{Bottom: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.base.message(gtx, message, danger)
				})
			}),
			layout.Flexed(1, ui.layoutItems),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					footer := material.Caption(ui.base.theme, "↑↓ Navigate    Enter Open pane    P Pause/Resume notifications    Esc Close")
					footer.Color = ui.base.colors.muted
					return footer.Layout(gtx)
				})
			}),
		)
	})
}

func (ui *attentionUI) layoutItems(gtx layout.Context) layout.Dimensions {
	if !ui.loaded {
		return ui.base.centeredMessage(gtx, "Connecting to zka…")
	}
	if len(ui.snapshot.Items) == 0 {
		return ui.base.centeredMessage(gtx, "Nothing needs your attention. Agents that are still working stay out of this view.")
	}
	return material.List(ui.base.theme, &ui.list).Layout(gtx, len(ui.snapshot.Items), func(gtx layout.Context, index int) layout.Dimensions {
		item := ui.snapshot.Items[index]
		title := item.WorkspaceName
		if item.PaneTitle != "" {
			title += "  /  " + item.PaneTitle
		}
		return layout.Inset{Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ui.base.actionCard(gtx, ui.row(item.ID), title, attentionItemSummary(item, gtx.Now), "Open →", ui.selected == index)
		})
	})
}

func attentionHeading(snapshot zka.AttentionSnapshot) string {
	if snapshot.Paused {
		return fmt.Sprintf("Attention paused · %d pending", snapshot.Counts.Total)
	}
	if snapshot.Counts.Total == 1 {
		return "1 pane needs you"
	}
	return fmt.Sprintf("%d panes need you", snapshot.Counts.Total)
}

func attentionSubtitle(snapshot zka.AttentionSnapshot) string {
	parts := []string{
		fmt.Sprintf("%d waiting", snapshot.Counts.Blocked),
		fmt.Sprintf("%d failed", snapshot.Counts.Error),
		fmt.Sprintf("%d finished", snapshot.Counts.Done),
	}
	if snapshot.Paused {
		return "Agents keep running; desktop and ntfy interruptions are deferred.  " + strings.Join(parts, " · ")
	}
	return "Only panes that currently need attention appear here.  " + strings.Join(parts, " · ")
}

func attentionItemSummary(item zka.AttentionItem, now time.Time) string {
	parts := []string{attentionItemState(item.State)}
	if item.Agent != "" {
		parts = append(parts, item.Agent)
	}
	if item.Origin != "" {
		parts = append(parts, item.Origin)
	}
	if !item.TransitionedAt.IsZero() {
		parts = append(parts, waitingAge(now.Sub(item.TransitionedAt)))
	}
	if item.Detail != "" {
		parts = append(parts, item.Detail)
	} else if item.Evidence != "" {
		parts = append(parts, item.Evidence)
	}
	return strings.Join(parts, "  ·  ")
}

func attentionItemState(state zka.AgentState) string {
	switch state {
	case zka.StateBlocked:
		return "Waiting for you"
	case zka.StateError:
		return "Failed"
	case zka.StateDone:
		return "Finished"
	default:
		return string(state)
	}
}

func waitingAge(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	if duration < time.Minute {
		return "just now"
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh", int(duration.Hours()))
	}
	return fmt.Sprintf("%dd", int(duration.Hours()/24))
}
