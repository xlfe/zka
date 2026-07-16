package launcher

import (
	"context"
	"image"
	"image/color"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/xlfe/zka/internal/zka"
)

type screen uint8

const (
	screenHome screen = iota
	screenCreate
	screenRemoteHost
	screenRemoteList
)

type resultKind uint8

const (
	resultLocal resultKind = iota
	resultRemote
	resultLaunch
)

type asyncResult struct {
	kind       resultKind
	token      uint64
	workspaces []*zka.Workspace
	err        error
}

type palette struct {
	background color.NRGBA
	surface    color.NRGBA
	selected   color.NRGBA
	muted      color.NRGBA
	accent     color.NRGBA
	danger     color.NRGBA
}

type ui struct {
	backend Backend
	window  *app.Window
	theme   *material.Theme
	colors  palette

	ctx             context.Context
	cancel          context.CancelFunc
	operationCancel context.CancelFunc
	operationToken  uint64
	operationKind   resultKind
	results         chan asyncResult

	screen       screen
	local        []*zka.Workspace
	remote       []*zka.Workspace
	remoteHosts  []string
	remoteHost   string
	selected     int
	localLoading bool
	localError   string
	busy         bool
	status       string
	errorMessage string
	focusPending *widget.Editor

	newButton     widget.Clickable
	remoteButton  widget.Clickable
	backButton    widget.Clickable
	primaryButton widget.Clickable
	retryButton   widget.Clickable
	rows          map[string]*widget.Clickable
	localList     widget.List
	remoteList    widget.List
	nameEditor    widget.Editor
	hostEditor    widget.Editor
}

func Run(w *app.Window) error {
	backend, err := newCommandBackend()
	if err != nil {
		backend = unavailableBackend{err: err}
	}
	application := newUI(backend)
	w.Option(
		app.Title("zka workspace launcher"),
		app.Size(unit.Dp(680), unit.Dp(560)),
	)
	return application.run(w)
}

type unavailableBackend struct{ err error }

func (b unavailableBackend) Workspaces(context.Context, string) ([]*zka.Workspace, error) {
	return nil, b.err
}

func (b unavailableBackend) Execute(context.Context, []string) error { return b.err }

func newUI(backend Backend) *ui {
	colors := palette{
		background: color.NRGBA{R: 0x0d, G: 0x11, B: 0x17, A: 0xff},
		surface:    color.NRGBA{R: 0x18, G: 0x20, B: 0x2b, A: 0xff},
		selected:   color.NRGBA{R: 0x22, G: 0x36, B: 0x43, A: 0xff},
		muted:      color.NRGBA{R: 0x99, G: 0xa8, B: 0xb8, A: 0xff},
		accent:     color.NRGBA{R: 0x6e, G: 0xd5, B: 0xc0, A: 0xff},
		danger:     color.NRGBA{R: 0xff, G: 0x8f, B: 0x91, A: 0xff},
	}
	theme := material.NewTheme()
	theme.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	theme.Palette = material.Palette{
		Bg:         colors.background,
		Fg:         color.NRGBA{R: 0xed, G: 0xf2, B: 0xf7, A: 0xff},
		ContrastBg: colors.accent,
		ContrastFg: colors.background,
	}
	application := &ui{
		backend: backend,
		theme:   theme,
		colors:  colors,
		results: make(chan asyncResult, 8),
		rows:    map[string]*widget.Clickable{},
	}
	application.localList.Axis = layout.Vertical
	application.remoteList.Axis = layout.Vertical
	application.nameEditor.SingleLine = true
	application.nameEditor.Submit = true
	application.hostEditor.SingleLine = true
	application.hostEditor.Submit = true
	return application
}

func (ui *ui) run(w *app.Window) error {
	ui.window = w
	ui.ctx, ui.cancel = context.WithCancel(context.Background())
	defer ui.cancel()
	ui.loadLocal()
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			if ui.operationCancel != nil {
				ui.operationCancel()
			}
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.drainResults()
			area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
			event.Op(gtx.Ops, w)
			ui.handleKeys(gtx)
			ui.handleEditorEvents(gtx)
			ui.handleClicks(gtx)
			if ui.focusPending != nil {
				gtx.Execute(key.FocusCmd{Tag: ui.focusPending})
				ui.focusPending = nil
			}
			ui.layout(gtx)
			area.Pop()
			e.Frame(gtx.Ops)
		}
	}
}

func (ui *ui) loadLocal() {
	ui.localLoading = true
	go func() {
		ctx, cancel := context.WithTimeout(ui.ctx, 10*time.Second)
		defer cancel()
		workspaces, err := ui.backend.Workspaces(ctx, "")
		ui.deliver(asyncResult{kind: resultLocal, workspaces: workspaces, err: err})
	}()
}

func (ui *ui) loadRemote(host string) {
	host = strings.TrimSpace(host)
	if host == "" {
		ui.errorMessage = "Enter an SSH host alias."
		return
	}
	ui.cancelOperation()
	ui.operationToken++
	token := ui.operationToken
	ui.remoteHost = host
	ui.remote = nil
	ui.screen = screenRemoteList
	ui.selected = 0
	ui.busy = true
	ui.operationKind = resultRemote
	ui.status = "Connecting to " + host + "…"
	ui.errorMessage = ""
	ctx, cancel := context.WithTimeout(ui.ctx, 20*time.Second)
	ui.operationCancel = cancel
	go func() {
		workspaces, err := ui.backend.Workspaces(ctx, host)
		ui.deliver(asyncResult{kind: resultRemote, token: token, workspaces: workspaces, err: err})
	}()
}

func (ui *ui) launch(args []string, status string) {
	if ui.busy {
		return
	}
	ui.cancelOperation()
	ui.operationToken++
	token := ui.operationToken
	ui.busy = true
	ui.operationKind = resultLaunch
	ui.status = status
	ui.errorMessage = ""
	ctx, cancel := context.WithTimeout(ui.ctx, 60*time.Second)
	ui.operationCancel = cancel
	go func() {
		err := ui.backend.Execute(ctx, args)
		ui.deliver(asyncResult{kind: resultLaunch, token: token, err: err})
	}()
}

func (ui *ui) cancelOperation() {
	if ui.operationCancel != nil {
		ui.operationCancel()
		ui.operationCancel = nil
	}
}

func (ui *ui) deliver(result asyncResult) {
	select {
	case ui.results <- result:
	case <-ui.ctx.Done():
		return
	}
	ui.window.Invalidate()
}

func (ui *ui) drainResults() {
	for {
		select {
		case result := <-ui.results:
			switch result.kind {
			case resultLocal:
				ui.localLoading = false
				if result.err != nil {
					ui.localError = "Could not load local workspaces: " + result.err.Error()
					continue
				}
				ui.local, ui.remoteHosts = splitWorkspaces(result.workspaces)
				ui.localError = ""
				ui.clampSelection()
			case resultRemote:
				if result.token != ui.operationToken {
					continue
				}
				ui.cancelOperation()
				ui.busy = false
				ui.status = ""
				if result.err != nil {
					ui.errorMessage = "Could not load " + ui.remoteHost + ": " + result.err.Error()
					continue
				}
				ui.remote = sortRemoteWorkspaces(result.workspaces)
				ui.errorMessage = ""
				ui.clampSelection()
			case resultLaunch:
				if result.token != ui.operationToken {
					continue
				}
				ui.cancelOperation()
				ui.busy = false
				ui.status = ""
				if result.err != nil {
					ui.errorMessage = result.err.Error()
					continue
				}
				ui.window.Perform(system.ActionClose)
			}
		default:
			return
		}
	}
}

func (ui *ui) handleKeys(gtx layout.Context) {
	filters := []event.Filter{key.Filter{Name: key.NameEscape}}
	if ui.screen == screenHome || ui.screen == screenRemoteList {
		filters = append(filters,
			key.Filter{Name: key.NameUpArrow},
			key.Filter{Name: key.NameDownArrow},
			key.Filter{Name: key.NameReturn},
			key.Filter{Name: key.NameEnter},
		)
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
			ui.back()
		case key.NameUpArrow:
			if ui.selected > 0 {
				ui.selected--
			}
		case key.NameDownArrow:
			if ui.selected+1 < ui.selectionCount() {
				ui.selected++
			}
		case key.NameReturn, key.NameEnter:
			ui.activateSelection()
		}
	}
}

func (ui *ui) handleEditorEvents(gtx layout.Context) {
	var editor *widget.Editor
	switch ui.screen {
	case screenCreate:
		editor = &ui.nameEditor
	case screenRemoteHost:
		editor = &ui.hostEditor
	default:
		return
	}
	for {
		raw, ok := editor.Update(gtx)
		if !ok {
			return
		}
		if _, submitted := raw.(widget.SubmitEvent); !submitted {
			continue
		}
		if ui.screen == screenCreate {
			ui.launch(createArgs(ui.nameEditor.Text()), "Creating workspace…")
		} else {
			ui.loadRemote(ui.hostEditor.Text())
		}
	}
}

func (ui *ui) handleClicks(gtx layout.Context) {
	if ui.backButton.Clicked(gtx) {
		ui.back()
		return
	}
	switch ui.screen {
	case screenHome:
		if ui.newButton.Clicked(gtx) {
			ui.openCreate()
			return
		}
		if ui.remoteButton.Clicked(gtx) {
			ui.openRemoteHost()
			return
		}
		if ui.retryButton.Clicked(gtx) && !ui.localLoading {
			ui.localError = ""
			ui.loadLocal()
			return
		}
		for _, workspace := range ui.local {
			if ui.row("local:" + workspace.ID).Clicked(gtx) {
				ui.launch(attachArgs("", workspace), "Attaching to "+workspace.Name+"…")
				return
			}
		}
	case screenCreate:
		if ui.primaryButton.Clicked(gtx) {
			ui.launch(createArgs(ui.nameEditor.Text()), "Creating workspace…")
		}
	case screenRemoteHost:
		if ui.primaryButton.Clicked(gtx) {
			ui.loadRemote(ui.hostEditor.Text())
			return
		}
		for _, host := range ui.remoteHosts {
			if ui.row("host:" + host).Clicked(gtx) {
				ui.hostEditor.SetText(host)
				ui.loadRemote(host)
				return
			}
		}
	case screenRemoteList:
		if ui.retryButton.Clicked(gtx) && !ui.busy {
			ui.loadRemote(ui.remoteHost)
			return
		}
		for _, workspace := range ui.remote {
			if ui.row("remote:" + ui.remoteHost + ":" + workspace.ID).Clicked(gtx) {
				ui.launch(attachArgs(ui.remoteHost, workspace), "Attaching to "+workspace.Name+"…")
				return
			}
		}
	}
}

func (ui *ui) row(key string) *widget.Clickable {
	button := ui.rows[key]
	if button == nil {
		button = new(widget.Clickable)
		ui.rows[key] = button
	}
	return button
}

func (ui *ui) openCreate() {
	if ui.busy {
		return
	}
	ui.screen = screenCreate
	ui.errorMessage = ""
	ui.status = ""
	ui.focusPending = &ui.nameEditor
}

func (ui *ui) openRemoteHost() {
	if ui.busy {
		return
	}
	ui.screen = screenRemoteHost
	ui.errorMessage = ""
	ui.status = ""
	ui.focusPending = &ui.hostEditor
}

func (ui *ui) back() {
	if ui.busy && ui.operationKind == resultLaunch {
		return
	}
	switch ui.screen {
	case screenHome:
		ui.window.Perform(system.ActionClose)
	case screenRemoteList:
		ui.cancelOperation()
		ui.operationToken++
		ui.busy = false
		ui.status = ""
		ui.errorMessage = ""
		ui.screen = screenRemoteHost
		ui.focusPending = &ui.hostEditor
	default:
		ui.errorMessage = ""
		ui.status = ""
		ui.screen = screenHome
		ui.clampSelection()
	}
}

func (ui *ui) activateSelection() {
	if ui.busy {
		return
	}
	switch ui.screen {
	case screenHome:
		switch ui.selected {
		case 0:
			ui.openCreate()
		case 1:
			ui.openRemoteHost()
		default:
			index := ui.selected - 2
			if index >= 0 && index < len(ui.local) {
				workspace := ui.local[index]
				ui.launch(attachArgs("", workspace), "Attaching to "+workspace.Name+"…")
			}
		}
	case screenRemoteList:
		if ui.selected >= 0 && ui.selected < len(ui.remote) {
			workspace := ui.remote[ui.selected]
			ui.launch(attachArgs(ui.remoteHost, workspace), "Attaching to "+workspace.Name+"…")
		}
	}
}

func (ui *ui) selectionCount() int {
	switch ui.screen {
	case screenHome:
		return 2 + len(ui.local)
	case screenRemoteList:
		return len(ui.remote)
	default:
		return 0
	}
}

func (ui *ui) clampSelection() {
	count := ui.selectionCount()
	if count == 0 {
		ui.selected = 0
	} else if ui.selected >= count {
		ui.selected = count - 1
	}
}

func (ui *ui) layout(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, ui.colors.background, clip.Rect{Max: gtx.Constraints.Max}.Op())
	return layout.Inset{Top: 28, Right: 30, Bottom: 20, Left: 30}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		switch ui.screen {
		case screenCreate:
			return ui.layoutCreate(gtx)
		case screenRemoteHost:
			return ui.layoutRemoteHost(gtx)
		case screenRemoteList:
			return ui.layoutRemoteList(gtx)
		default:
			return ui.layoutHome(gtx)
		}
	})
}

func (ui *ui) layoutHome(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "Workspaces", "Attach to a running workspace or start a new one.", false, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.actionRow(gtx, &ui.newButton, "New workspace", "Start a new managed Kitty workspace", ui.selected == 0)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.actionRow(gtx, &ui.remoteButton, "Remote workspace", "Connect through an SSH host alias", ui.selected == 1)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 14, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					label := material.Subtitle2(ui.theme, "LOCAL WORKSPACES")
					label.Color = ui.colors.muted
					return label.Layout(gtx)
				})
			}),
			layout.Flexed(1, ui.layoutLocalList),
		)
	})
}

func (ui *ui) layoutLocalList(gtx layout.Context) layout.Dimensions {
	if ui.localLoading {
		return ui.centeredMessage(gtx, "Loading local workspaces…")
	}
	if len(ui.local) == 0 {
		message := "No local workspaces yet."
		if ui.localError != "" {
			message = ui.localError
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.message(gtx, message, ui.localError != "") }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if ui.localError == "" {
					return layout.Dimensions{}
				}
				return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.secondaryButton(gtx, &ui.retryButton, "Retry")
				})
			}),
		)
	}
	return material.List(ui.theme, &ui.localList).Layout(gtx, len(ui.local), func(gtx layout.Context, index int) layout.Dimensions {
		workspace := ui.local[index]
		return ui.workspaceRow(gtx, ui.row("local:"+workspace.ID), workspace, ui.selected == index+2)
	})
}

func (ui *ui) layoutCreate(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "New workspace", "Give it a name, or leave the field blank for an automatic one.", true, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Subtitle2(ui.theme, "WORKSPACE NAME (OPTIONAL)")
				label.Color = ui.colors.muted
				return label.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.editor(gtx, &ui.nameEditor, "e.g. example-project")
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.operationMessage(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.primary(gtx, &ui.primaryButton, "Create workspace")
				})
			}),
		)
	})
}

func (ui *ui) layoutRemoteHost(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "Remote workspace", "Enter an OpenSSH host alias known to this machine.", true, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Subtitle2(ui.theme, "SSH HOST ALIAS")
				label.Color = ui.colors.muted
				return label.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.editor(gtx, &ui.hostEditor, "e.g. devbox.example")
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.operationMessage(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.primary(gtx, &ui.primaryButton, "List remote workspaces")
				})
			}),
		}
		if len(ui.remoteHosts) > 0 {
			children = append(children,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: 24, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						label := material.Subtitle2(ui.theme, "RECENT HOSTS")
						label.Color = ui.colors.muted
						return label.Layout(gtx)
					})
				}),
			)
			for _, host := range ui.remoteHosts {
				host := host
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.actionRow(gtx, ui.row("host:"+host), host, "Previously connected SSH host", false)
				}))
			}
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (ui *ui) layoutRemoteList(gtx layout.Context) layout.Dimensions {
	title := "Workspaces on " + ui.remoteHost
	return ui.page(gtx, title, "Choose a workspace to attach on this machine.", true, func(gtx layout.Context) layout.Dimensions {
		if ui.busy {
			return ui.centeredMessage(gtx, ui.status)
		}
		if ui.errorMessage != "" {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.message(gtx, ui.errorMessage, true) }),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return ui.secondaryButton(gtx, &ui.retryButton, "Retry")
					})
				}),
			)
		}
		if len(ui.remote) == 0 {
			return ui.centeredMessage(gtx, "No workspaces found on "+ui.remoteHost+".")
		}
		return material.List(ui.theme, &ui.remoteList).Layout(gtx, len(ui.remote), func(gtx layout.Context, index int) layout.Dimensions {
			workspace := ui.remote[index]
			return ui.workspaceRow(gtx, ui.row("remote:"+ui.remoteHost+":"+workspace.ID), workspace, ui.selected == index)
		})
	})
}

func (ui *ui) page(gtx layout.Context, title, subtitle string, back bool, content layout.Widget) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					brand := material.H6(ui.theme, "zka")
					brand.Color = ui.colors.accent
					return brand.Layout(gtx)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{} }),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !back {
						return layout.Dimensions{}
					}
					return ui.secondaryButton(gtx, &ui.backButton, "← Back")
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				heading := material.H5(ui.theme, title)
				return heading.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 5, Bottom: 22}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(ui.theme, subtitle)
				label.Color = ui.colors.muted
				return label.Layout(gtx)
			})
		}),
		layout.Flexed(1, content),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if ui.screen != screenHome && ui.screen != screenRemoteList {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 12}.Layout(gtx, ui.footer)
		}),
	)
}

func (ui *ui) workspaceRow(gtx layout.Context, button *widget.Clickable, workspace *zka.Workspace, selected bool) layout.Dimensions {
	return ui.actionRow(gtx, button, workspace.Name, workspaceSummary(workspace), selected)
}

func (ui *ui) actionRow(gtx layout.Context, button *widget.Clickable, title, subtitle string, selected bool) layout.Dimensions {
	return layout.Inset{Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		background := ui.colors.surface
		if selected {
			background = ui.colors.selected
		}
		style := material.ButtonLayout(ui.theme, button)
		style.Background = background
		style.CornerRadius = 12
		return style.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Top: 12, Right: 14, Bottom: 12, Left: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := material.Subtitle1(ui.theme, title)
						if selected {
							label.Color = ui.colors.accent
						}
						return label.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: 3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							label := material.Caption(ui.theme, subtitle)
							label.Color = ui.colors.muted
							return label.Layout(gtx)
						})
					}),
				)
			})
		})
	})
}

func (ui *ui) editor(gtx layout.Context, editor *widget.Editor, hint string) layout.Dimensions {
	style := material.Editor(ui.theme, editor, hint)
	style.HintColor = ui.colors.muted
	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			paint.FillShape(gtx.Ops, ui.colors.surface, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(10)).Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Top: 12, Right: 14, Bottom: 12, Left: 14}.Layout(gtx, style.Layout)
		},
	)
}

func (ui *ui) primary(gtx layout.Context, button *widget.Clickable, label string) layout.Dimensions {
	style := material.Button(ui.theme, button, label)
	style.Background = ui.colors.accent
	style.Color = ui.colors.background
	style.CornerRadius = 10
	style.Inset = layout.Inset{Top: 12, Right: 18, Bottom: 12, Left: 18}
	return style.Layout(gtx)
}

func (ui *ui) secondaryButton(gtx layout.Context, button *widget.Clickable, label string) layout.Dimensions {
	style := material.Button(ui.theme, button, label)
	style.Background = ui.colors.surface
	style.Color = ui.theme.Palette.Fg
	style.CornerRadius = 9
	style.Inset = layout.Inset{Top: 8, Right: 12, Bottom: 8, Left: 12}
	return style.Layout(gtx)
}

func (ui *ui) operationMessage(gtx layout.Context) layout.Dimensions {
	if ui.errorMessage != "" {
		return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ui.message(gtx, ui.errorMessage, true)
		})
	}
	if ui.status != "" {
		return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ui.message(gtx, ui.status, false)
		})
	}
	return layout.Dimensions{}
}

func (ui *ui) message(gtx layout.Context, message string, danger bool) layout.Dimensions {
	label := material.Body2(ui.theme, message)
	label.Color = ui.colors.muted
	if danger {
		label.Color = ui.colors.danger
	}
	return label.Layout(gtx)
}

func (ui *ui) centeredMessage(gtx layout.Context, message string) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return ui.message(gtx, message, false)
	})
}

func (ui *ui) footer(gtx layout.Context) layout.Dimensions {
	label := material.Caption(ui.theme, "↑↓ Navigate    Enter Select    Esc Back")
	label.Color = ui.colors.muted
	return label.Layout(gtx)
}
