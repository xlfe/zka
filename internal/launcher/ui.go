package launcher

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"sort"
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
	screenRemoteCreate
	screenForget
)

type resultKind uint8

const (
	resultLocal resultKind = iota
	resultRemote
	resultLaunch
	resultDetach
	resultCredentials
	resultForget
)

type asyncResult struct {
	kind       resultKind
	token      uint64
	node       zka.Host
	workspace  string
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

	screen                screen
	local                 []*zka.Workspace
	remote                []*zka.Workspace
	remoteHosts           []string
	remoteHost            string
	localNodeID           string
	selectOnLoad          string
	selected              int
	credentialsEnabled    bool
	defaultBundle         string
	localLoading          bool
	localError            string
	busy                  bool
	status                string
	errorMessage          string
	focusPending          *widget.Editor
	forgetWorkspace       *zka.Workspace
	forgetReturnSelection int

	newButton         widget.Clickable
	remoteButton      widget.Clickable
	backButton        widget.Clickable
	primaryButton     widget.Clickable
	retryButton       widget.Clickable
	rows              map[string]*widget.Clickable
	selectables       map[string]*widget.Selectable
	localList         widget.List
	remoteList        widget.List
	remoteCredentials widget.Bool
	createCredentials widget.Bool
	nameEditor        widget.Editor
	hostEditor        widget.Editor
	remoteNameEditor  widget.Editor
}

func Run(w *app.Window, modes ...string) error {
	backend, err := newCommandBackend()
	if err != nil {
		backend = unavailableBackend{err: err}
	}
	if len(modes) > 1 || (len(modes) == 1 && modes[0] != "attention") {
		return fmt.Errorf("unknown launcher mode %q", strings.Join(modes, " "))
	}
	if len(modes) == 1 {
		application := newAttentionUI(backend)
		w.Option(
			app.Title("zka attention"),
			app.Size(unit.Dp(720), unit.Dp(560)),
		)
		return application.run(w)
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

func (b unavailableBackend) Node(context.Context) (zka.Host, error) { return zka.Host{}, b.err }

func (b unavailableBackend) Attention(context.Context) (zka.AttentionSnapshot, error) {
	return zka.AttentionSnapshot{}, b.err
}

func (b unavailableBackend) WatchAttention(context.Context, func(zka.AttentionSnapshot) error) error {
	return b.err
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
	credentialsEnabled := false
	defaultBundle := ""
	if cfg, err := zka.LoadConfig(); err == nil {
		credentialsEnabled = len(cfg.Credentials.Bundles) != 0
		defaultBundle = cfg.Credentials.DefaultBundle
	}
	application := &ui{
		backend: backend, theme: theme, colors: colors,
		credentialsEnabled: credentialsEnabled, defaultBundle: defaultBundle,
		results: make(chan asyncResult, 8), rows: map[string]*widget.Clickable{},
		selectables: map[string]*widget.Selectable{},
	}
	application.localList.Axis = layout.Vertical
	application.remoteList.Axis = layout.Vertical
	application.nameEditor.SingleLine = true
	application.nameEditor.Submit = true
	application.hostEditor.SingleLine = true
	application.hostEditor.Submit = true
	application.remoteNameEditor.SingleLine = true
	application.remoteNameEditor.Submit = true
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
		node, err := ui.backend.Node(ctx)
		if err != nil {
			ui.deliver(asyncResult{kind: resultLocal, err: err})
			return
		}
		workspaces, err := ui.backend.Workspaces(ctx, "")
		ui.deliver(asyncResult{kind: resultLocal, node: node, workspaces: workspaces, err: err})
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
	ui.remoteCredentials.Value = false
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
	ui.execute(resultLaunch, "", args, status)
}

func (ui *ui) launchAll(commands [][]string, status string) {
	ui.executeAll(resultLaunch, "", commands, status)
}

func (ui *ui) detachWorkspace(workspace *zka.Workspace) {
	ui.execute(resultDetach, workspace.ID, detachArgs(workspace), "Detaching "+workspace.Name+"…")
}

func (ui *ui) openForget(workspace *zka.Workspace) {
	if ui.busy || !workspaceForgettable(workspace, ui.localNodeID) {
		return
	}
	ui.forgetWorkspace = workspace
	ui.forgetReturnSelection = ui.selected
	ui.screen = screenForget
	ui.status = ""
	ui.errorMessage = ""
}

func (ui *ui) confirmForget() {
	if ui.forgetWorkspace == nil {
		return
	}
	workspace := ui.forgetWorkspace
	ui.execute(resultForget, workspace.ID, forgetArgs(workspace), "Forgetting "+workspace.Name+" on this machine…")
}

func (ui *ui) toggleWorkspaceCredentials(workspace *zka.Workspace) {
	if workspace.RemoteHost == "" {
		if workspace.CredentialClaim == nil {
			return
		}
		releaseArgs, _ := workspaceCredentialAction(workspace, ui.localNodeID)
		if !workspaceAttachedToNode(workspace, ui.localNodeID) {
			ui.launchAll(
				[][]string{releaseArgs, attachArgs("", workspace)},
				"Releasing remote credentials and attaching to "+workspace.Name+"…",
			)
			return
		}
		ui.execute(
			resultCredentials,
			workspace.ID,
			releaseArgs,
			"Releasing credentials from "+workspace.Name+"…",
		)
		return
	}
	if !workspaceAttachedToNode(workspace, ui.localNodeID) &&
		!workspaceCredentialsClaimedByNode(workspace, ui.localNodeID) {
		ui.launch(
			remoteAttachArgs(workspace.RemoteHost, workspace, true, ui.defaultBundle),
			"Attaching to "+workspace.Name+" and claiming "+ui.defaultBundle+" credentials…",
		)
		return
	}
	args, action := workspaceCredentialAction(workspace, ui.localNodeID)
	ui.execute(resultCredentials, workspace.ID, args, action+" "+workspace.Name+"…")
}

func (ui *ui) execute(kind resultKind, workspace string, args []string, status string) {
	ui.executeAll(kind, workspace, [][]string{args}, status)
}

func (ui *ui) executeAll(kind resultKind, workspace string, commands [][]string, status string) {
	if ui.busy {
		return
	}
	ui.cancelOperation()
	ui.operationToken++
	token := ui.operationToken
	ui.busy = true
	ui.operationKind = kind
	ui.status = status
	ui.errorMessage = ""
	ctx, cancel := context.WithTimeout(ui.ctx, 60*time.Second)
	ui.operationCancel = cancel
	go func() {
		var err error
		for _, args := range commands {
			if err = ui.backend.Execute(ctx, args); err != nil {
				break
			}
		}
		ui.deliver(asyncResult{kind: kind, token: token, workspace: workspace, err: err})
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
					ui.localError = "Could not load workspaces: " + result.err.Error()
					ui.selectOnLoad = ""
					continue
				}
				ui.localNodeID = result.node.ID
				ui.local, ui.remoteHosts = splitWorkspaces(result.workspaces, ui.localNodeID)
				ui.localError = ""
				if ui.selectOnLoad != "" {
					for index, workspace := range ui.local {
						if workspace.ID == ui.selectOnLoad {
							ui.selected = index + 2
							break
						}
					}
					ui.selectOnLoad = ""
				}
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
				// Land on the first workspace; the New row stays one step up.
				if len(ui.remote) != 0 && ui.selected == 0 {
					ui.selected = 1
				}
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
			case resultDetach, resultCredentials:
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
				ui.errorMessage = ""
				ui.selectOnLoad = result.workspace
				ui.loadLocal()
			case resultForget:
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
				ui.errorMessage = ""
				ui.forgetWorkspace = nil
				ui.screen = screenHome
				ui.selected = ui.forgetReturnSelection
				ui.loadLocal()
			}
		default:
			return
		}
	}
}

func (ui *ui) handleKeys(gtx layout.Context) {
	filters := []event.Filter{key.Filter{Name: key.NameEscape}}
	if ui.screen == screenHome || ui.screen == screenRemoteList || ui.screen == screenForget {
		filters = append(filters,
			key.Filter{Name: key.NameReturn},
			key.Filter{Name: key.NameEnter},
		)
	}
	if ui.screen == screenHome || ui.screen == screenRemoteList {
		filters = append(filters,
			key.Filter{Name: key.NameUpArrow},
			key.Filter{Name: key.NameDownArrow},
		)
	}
	if ui.screen == screenHome {
		filters = append(filters,
			key.Filter{Name: "D"},
			key.Filter{Name: "F"},
			key.Filter{Name: key.NameDeleteForward},
		)
	}
	if ui.screen == screenHome || ui.screen == screenRemoteList {
		filters = append(filters, key.Filter{Name: "A"})
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
			ui.moveSelection(-1)
		case key.NameDownArrow:
			ui.moveSelection(1)
		case key.NameReturn, key.NameEnter:
			ui.activateSelection()
		case "D", key.NameDeleteForward:
			ui.detachSelection()
		case "F":
			ui.forgetSelection()
		case "A":
			ui.toggleCredentialSelection()
		}
	}
}

func (ui *ui) moveSelection(delta int) {
	next := ui.selected + delta
	if next < 0 || next >= ui.selectionCount() {
		return
	}
	ui.selected = next
	ui.scrollSelectionIntoView()
}

func (ui *ui) scrollSelectionIntoView() {
	list, item, ok := ui.selectedListItem()
	if !ok {
		return
	}
	position := list.Position
	if position.Count == 0 {
		return
	}
	last := position.First + position.Count - 1
	if item < position.First || item > last ||
		(item == position.First && position.Offset > 0) ||
		(item == last && position.OffsetLast < 0) {
		list.ScrollTo(item)
	}
}

func (ui *ui) selectedListItem() (*widget.List, int, bool) {
	switch ui.screen {
	case screenHome:
		selection := ui.selected - 2
		if selection < 0 {
			return nil, 0, false
		}
		for index, item := range localWorkspaceItems(ui.local, ui.localNodeID) {
			if item.workspace != nil && item.selection == selection {
				return &ui.localList, index, true
			}
		}
	case screenRemoteList:
		if ui.selected > 0 {
			return &ui.remoteList, ui.selected - 1, true
		}
	}
	return nil, 0, false
}

func (ui *ui) handleEditorEvents(gtx layout.Context) {
	var editor *widget.Editor
	switch ui.screen {
	case screenCreate:
		editor = &ui.nameEditor
	case screenRemoteHost:
		editor = &ui.hostEditor
	case screenRemoteCreate:
		editor = &ui.remoteNameEditor
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
		switch ui.screen {
		case screenCreate:
			ui.launch(createArgs(ui.nameEditor.Text(), ui.createCredentials.Value), "Creating workspace…")
		case screenRemoteHost:
			ui.loadRemote(ui.hostEditor.Text())
		case screenRemoteCreate:
			ui.createRemote()
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
			key := "home:" + workspace.RemoteHost + ":" + workspace.ID
			if workspaceForgettable(workspace, ui.localNodeID) && ui.row("forget:"+key).Clicked(gtx) {
				ui.openForget(workspace)
				return
			}
			if ui.workspaceCredentialControlVisible(workspace) && ui.row("credentials:"+key).Clicked(gtx) {
				ui.toggleWorkspaceCredentials(workspace)
				return
			}
			if workspaceAttachedToNode(workspace, ui.localNodeID) && ui.row("detach:"+key).Clicked(gtx) {
				ui.detachWorkspace(workspace)
				return
			}
			if ui.row(key).Clicked(gtx) {
				ui.activateWorkspace(workspace)
				return
			}
		}
	case screenCreate:
		if ui.primaryButton.Clicked(gtx) {
			ui.launch(createArgs(ui.nameEditor.Text(), ui.createCredentials.Value), "Creating workspace…")
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
		if ui.row("remote-list:new:" + ui.remoteHost).Clicked(gtx) {
			ui.openRemoteCreate()
			return
		}
		for _, workspace := range ui.remote {
			if ui.row("remote:" + ui.remoteHost + ":" + workspace.ID).Clicked(gtx) {
				ui.launch(remoteAttachArgs(ui.remoteHost, workspace, ui.remoteCredentials.Value, ui.defaultBundle), "Attaching to "+workspace.Name+"…")
				return
			}
		}
	case screenRemoteCreate:
		if ui.primaryButton.Clicked(gtx) {
			ui.createRemote()
		}
	case screenForget:
		if ui.primaryButton.Clicked(gtx) {
			ui.confirmForget()
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

func (ui *ui) selectable(key string) *widget.Selectable {
	state := ui.selectables[key]
	if state == nil {
		state = new(widget.Selectable)
		ui.selectables[key] = state
	}
	return state
}

func (ui *ui) selectableLabel(gtx layout.Context, key string, label material.LabelStyle) layout.Dimensions {
	label.State = ui.selectable(key)
	return label.Layout(gtx)
}

func (ui *ui) openCreate() {
	if ui.busy {
		return
	}
	ui.screen = screenCreate
	ui.createCredentials.Value = ui.credentialsEnabled && ui.defaultBundle != ""
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

func (ui *ui) openRemoteCreate() {
	if ui.busy {
		return
	}
	ui.screen = screenRemoteCreate
	ui.remoteCredentials.Value = ui.credentialsEnabled && ui.defaultBundle != ""
	ui.errorMessage = ""
	ui.status = ""
	ui.focusPending = &ui.remoteNameEditor
}

// createRemote births the workspace on the origin and attaches it here in one
// subprocess; success closes the launcher exactly like a plain attach.
func (ui *ui) createRemote() {
	ui.launch(remoteCreateArgs(ui.remoteHost, ui.remoteNameEditor.Text(), ui.remoteCredentials.Value, ui.defaultBundle), "Creating workspace on "+ui.remoteHost+"…")
}

func remoteAttachArgs(host string, workspace *zka.Workspace, claimCredentials bool, bundle string) []string {
	args := attachArgs(host, workspace)
	if claimCredentials {
		args = append(args, "--claim-credentials")
		if bundle != "" {
			args = append(args, "--credential-bundle", bundle)
		}
	}
	return args
}

func remoteCreateArgs(host, name string, claimCredentials bool, bundle string) []string {
	args := createRemoteArgs(host, name)
	if claimCredentials {
		if bundle != "" {
			args = append(args, "--credential-bundle", bundle)
		}
	} else {
		args = append(args, "--no-credentials")
	}
	return args
}

func workspaceCredentialsClaimedByNode(workspace *zka.Workspace, nodeID string) bool {
	if workspace == nil || workspace.CredentialClaim == nil || nodeID == "" {
		return false
	}
	return workspace.CredentialClaim.OwnerNodeID == nodeID
}

func workspaceCredentialAction(workspace *zka.Workspace, nodeID string) ([]string, string) {
	ref := workspace.ID
	if workspace.RemoteHost != "" {
		ref = workspace.RemoteHost + ":" + ref
	}
	if workspace.RemoteHost == "" {
		return []string{"workspace", "credentials", "release", ref}, "Releasing credentials from"
	}
	if workspaceCredentialsClaimedByNode(workspace, nodeID) {
		return []string{"workspace", "credentials", "release", ref}, "Releasing credentials from"
	}
	return []string{"workspace", "credentials", "claim", ref}, "Claiming credentials for"
}

func workspaceCredentialButtonLabel(workspace *zka.Workspace, nodeID string) string {
	if workspace.RemoteHost == "" && workspace.CredentialClaim != nil {
		if !workspaceAttachedToNode(workspace, nodeID) {
			return "Attach + release credentials"
		}
		return "Release credentials"
	}
	if workspaceCredentialsClaimedByNode(workspace, nodeID) {
		return "Release credentials"
	}
	if !workspaceAttachedToNode(workspace, nodeID) {
		return "Attach + claim credentials"
	}
	return "Claim credentials here"
}

func (ui *ui) workspaceCredentialControlVisible(workspace *zka.Workspace) bool {
	if workspace == nil {
		return false
	}
	if workspace.RemoteHost == "" {
		return workspace.CredentialClaim != nil
	}
	if workspaceCredentialsClaimedByNode(workspace, ui.localNodeID) {
		return true
	}
	return ui.credentialsEnabled && ui.defaultBundle != "" && workspaceKnownToNode(workspace, ui.localNodeID)
}

func workspaceCredentialSummary(workspace *zka.Workspace, remoteHost, localNodeID string) string {
	if workspace == nil {
		return ""
	}
	if workspace.CredentialClaim == nil {
		if remoteHost == "" {
			return ""
		}
		return "Credentials: unclaimed"
	}
	claim := workspace.CredentialClaim
	capabilities := make([]string, 0, len(claim.Capabilities))
	for name, capability := range claim.Capabilities {
		capabilities = append(capabilities, name+" "+capability.State)
	}
	sort.Strings(capabilities)
	owner := "another machine"
	if localNodeID != "" && claim.OwnerNodeID == localNodeID {
		owner = "this machine"
	} else {
		for _, attachment := range workspace.Attachments {
			if attachment != nil && attachment.Node.ID == claim.OwnerNodeID {
				owner = strings.TrimSpace(attachment.Node.Name)
				if owner == "" {
					owner = shortID(claim.OwnerNodeID)
				}
				break
			}
		}
	}
	if owner == "" {
		owner = "another machine"
	}
	detail := claim.Bundle
	if len(capabilities) != 0 {
		detail += " (" + strings.Join(capabilities, ", ") + ")"
	}
	return "Credentials: " + detail + " · " + owner
}

func (ui *ui) back() {
	if ui.busy && (ui.operationKind == resultLaunch || ui.operationKind == resultDetach || ui.operationKind == resultForget) {
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
	case screenRemoteCreate:
		ui.errorMessage = ""
		ui.status = ""
		// Existing-workspace attachment is deliberately opt-in even when the
		// create screen defaulted credentials on.
		ui.remoteCredentials.Value = false
		ui.screen = screenRemoteList
		ui.clampSelection()
	case screenForget:
		ui.errorMessage = ""
		ui.status = ""
		ui.forgetWorkspace = nil
		ui.screen = screenHome
		ui.selected = ui.forgetReturnSelection
		ui.clampSelection()
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
				ui.activateWorkspace(ui.local[index])
			}
		}
	case screenRemoteList:
		if ui.selected == 0 {
			ui.openRemoteCreate()
			return
		}
		index := ui.selected - 1
		if index >= 0 && index < len(ui.remote) {
			workspace := ui.remote[index]
			ui.launch(remoteAttachArgs(ui.remoteHost, workspace, ui.remoteCredentials.Value, ui.defaultBundle), "Attaching to "+workspace.Name+"…")
		}
	case screenForget:
		ui.confirmForget()
	}
}

func (ui *ui) activateWorkspace(workspace *zka.Workspace) {
	attached := workspaceAttachedToNode(workspace, ui.localNodeID)
	status := "Attaching to " + workspace.Name + "…"
	if attached {
		status = "Switching to " + workspace.Name + "…"
	}
	ui.launch(attachArgs(workspace.RemoteHost, workspace), status)
}

func (ui *ui) detachSelection() {
	if ui.busy || ui.screen != screenHome {
		return
	}
	index := ui.selected - 2
	if index < 0 || index >= len(ui.local) {
		return
	}
	workspace := ui.local[index]
	if workspaceAttachedToNode(workspace, ui.localNodeID) {
		ui.detachWorkspace(workspace)
	}
}

func (ui *ui) forgetSelection() {
	if ui.busy || ui.screen != screenHome {
		return
	}
	index := ui.selected - 2
	if index < 0 || index >= len(ui.local) {
		return
	}
	ui.openForget(ui.local[index])
}

func (ui *ui) toggleCredentialSelection() {
	if ui.busy {
		return
	}
	if ui.screen == screenCreate {
		if ui.credentialsEnabled && ui.defaultBundle != "" {
			ui.createCredentials.Value = !ui.createCredentials.Value
		}
		return
	}
	if ui.screen == screenRemoteList {
		if ui.credentialsEnabled && ui.defaultBundle != "" {
			ui.remoteCredentials.Value = !ui.remoteCredentials.Value
		}
		return
	}
	if ui.screen != screenHome {
		return
	}
	index := ui.selected - 2
	if index < 0 || index >= len(ui.local) {
		return
	}
	workspace := ui.local[index]
	if ui.workspaceCredentialControlVisible(workspace) {
		ui.toggleWorkspaceCredentials(workspace)
	}
}

func (ui *ui) selectionCount() int {
	switch ui.screen {
	case screenHome:
		return 2 + len(ui.local)
	case screenRemoteList:
		// Row 0 is "New workspace on <host>"; workspaces follow.
		return 1 + len(ui.remote)
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
		case screenRemoteCreate:
			return ui.layoutRemoteCreate(gtx)
		case screenForget:
			return ui.layoutForget(gtx)
		default:
			return ui.layoutHome(gtx)
		}
	})
}

func (ui *ui) layoutHome(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "Workspaces", "Switch attached workspaces, attach detached ones, or start a new one.", false, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.actionRow(gtx, "home:new-workspace", &ui.newButton, "New workspace", "Start a new managed Kitty workspace", ui.selected == 0)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.actionRow(gtx, "home:remote-workspace", &ui.remoteButton, "Remote workspace", "Connect through an SSH host alias", ui.selected == 1)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.operationMessage(gtx) }),
			layout.Flexed(1, ui.layoutLocalList),
		)
	})
}

func (ui *ui) layoutLocalList(gtx layout.Context) layout.Dimensions {
	if ui.localLoading {
		return ui.centeredMessage(gtx, "Loading local workspaces…")
	}
	if len(ui.local) == 0 {
		message := "No local or previously connected workspaces yet."
		if ui.localError != "" {
			message = ui.localError
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.message(gtx, "home:local-message", message, ui.localError != "")
			}),
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
	items := localWorkspaceItems(ui.local, ui.localNodeID)
	return material.List(ui.theme, &ui.localList).Layout(gtx, len(items), func(gtx layout.Context, index int) layout.Dimensions {
		item := items[index]
		if item.workspace == nil {
			return ui.workspaceSectionHeader(gtx, item.label)
		}
		workspace := item.workspace
		attached := workspaceAttachedToNode(workspace, ui.localNodeID)
		key := "home:" + workspace.RemoteHost + ":" + workspace.ID
		action := "Attach →"
		var detachButton *widget.Clickable
		if attached {
			action = "Switch →"
			detachButton = ui.row("detach:" + key)
		}
		var credentialButton *widget.Clickable
		credentialLabel := ""
		if ui.workspaceCredentialControlVisible(workspace) {
			credentialButton = ui.row("credentials:" + key)
			credentialLabel = workspaceCredentialButtonLabel(workspace, ui.localNodeID)
		}
		var forgetButton *widget.Clickable
		if workspaceForgettable(workspace, ui.localNodeID) {
			forgetButton = ui.row("forget:" + key)
		}
		return ui.workspaceRow(gtx, ui.row(key), detachButton, credentialButton, forgetButton, credentialLabel, workspace, action, ui.selected == item.selection+2)
	})
}

func (ui *ui) layoutCreate(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "New workspace", "Give it a name, or leave the field blank for an automatic one.", true, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Subtitle2(ui.theme, "WORKSPACE NAME (OPTIONAL)")
				label.Color = ui.colors.muted
				return ui.selectableLabel(gtx, "create:name-label", label)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.editor(gtx, &ui.nameEditor, "e.g. example-project")
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.layoutCreateCredentialOption(gtx) }),
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
				return ui.selectableLabel(gtx, "remote-host:alias-label", label)
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
						return ui.selectableLabel(gtx, "remote-host:recent-hosts", label)
					})
				}),
			)
			for _, host := range ui.remoteHosts {
				host := host
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.actionRow(gtx, "host:"+host, ui.row("host:"+host), host, "Previously connected SSH host", false)
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
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.message(gtx, "remote-list:error", ui.errorMessage, true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return ui.secondaryButton(gtx, &ui.retryButton, "Retry")
					})
				}),
			)
		}
		newRow := func(gtx layout.Context) layout.Dimensions {
			key := "remote-list:new:" + ui.remoteHost
			return ui.actionRow(gtx, key, ui.row(key), "New workspace on "+ui.remoteHost, "Create it on the origin and attach it here", ui.selected == 0)
		}
		credentialOption := func(gtx layout.Context) layout.Dimensions {
			return ui.layoutRemoteCredentialOption(gtx, "remote-list")
		}
		if len(ui.remote) == 0 {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(credentialOption),
				layout.Rigid(newRow),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.message(gtx, "remote-list:empty", "No workspaces found on "+ui.remoteHost+".", false)
				}),
			)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(credentialOption),
			layout.Rigid(newRow),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return material.List(ui.theme, &ui.remoteList).Layout(gtx, len(ui.remote), func(gtx layout.Context, index int) layout.Dimensions {
					workspace := ui.remote[index]
					return ui.workspaceRow(gtx, ui.row("remote:"+ui.remoteHost+":"+workspace.ID), nil, nil, nil, "", workspace, "Attach →", ui.selected == index+1)
				})
			}),
		)
	})
}

func (ui *ui) layoutRemoteCreate(gtx layout.Context) layout.Dimensions {
	return ui.page(gtx, "New workspace on "+ui.remoteHost, "Give it a name, or leave the field blank for an automatic one.", true, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Subtitle2(ui.theme, "WORKSPACE NAME (OPTIONAL)")
				label.Color = ui.colors.muted
				return ui.selectableLabel(gtx, "remote-create:name-label", label)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.editor(gtx, &ui.remoteNameEditor, "e.g. api-server")
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.layoutRemoteCredentialOption(gtx, "remote-create")
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.operationMessage(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.primary(gtx, &ui.primaryButton, "Create and attach")
				})
			}),
		)
	})
}

func (ui *ui) layoutForget(gtx layout.Context) layout.Dimensions {
	workspace := ui.forgetWorkspace
	if workspace == nil {
		return ui.page(gtx, "Forget workspace", "The selected workspace is no longer available.", true, ui.operationMessage)
	}
	title := "Forget " + workspace.Name + "?"
	subtitle := "Remove this machine's cached copy. The workspace and its processes on " + workspace.RemoteHost + " are untouched."
	return ui.page(gtx, title, subtitle, true, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ui.message(gtx, "forget:details",
					"Generated Kitty sessions on this machine will be removed. Credential ownership on the origin will not change, and reconnecting may make this workspace appear again.", false)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return ui.operationMessage(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ui.dangerButton(gtx, &ui.primaryButton, "Forget local copy")
				})
			}),
		)
	})
}

func (ui *ui) layoutRemoteCredentialOption(gtx layout.Context, key string) layout.Dimensions {
	return layout.Inset{Top: 12, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if !ui.credentialsEnabled || ui.defaultBundle == "" {
			ui.remoteCredentials.Value = false
			detail := "Set a default credential bundle to claim credentials while attaching."
			if ui.screen == screenRemoteCreate {
				detail = "No default credential bundle is configured; this workspace will start unclaimed."
			}
			return ui.message(gtx, key+":credentials-disabled", detail, false)
		}
		creating := ui.screen == screenRemoteCreate
		labelText := "Claim " + ui.defaultBundle + " credentials from this machine"
		helpText := "Credentials change only because this is an explicit attachment claim."
		if creating {
			labelText = "Use " + ui.defaultBundle + " credentials from this machine"
			helpText = "The creating machine remains the provider until release or an explicit transfer."
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				style := material.CheckBox(ui.theme, &ui.remoteCredentials, labelText)
				return style.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 34}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					label := material.Caption(ui.theme, helpText)
					label.Color = ui.colors.muted
					return ui.selectableLabel(gtx, key+":credentials-help", label)
				})
			}),
		)
	})
}

func (ui *ui) layoutCreateCredentialOption(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: 12, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if !ui.credentialsEnabled || ui.defaultBundle == "" {
			ui.createCredentials.Value = false
			return ui.message(gtx, "create:credentials-disabled", "No default credential bundle is configured; this workspace will start unclaimed.", false)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.CheckBox(ui.theme, &ui.createCredentials, "Use "+ui.defaultBundle+" credentials from this machine").Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 34}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					label := material.Caption(ui.theme, "The creating machine remains the provider until release or an explicit transfer.")
					label.Color = ui.colors.muted
					return ui.selectableLabel(gtx, "create:credentials-help", label)
				})
			}),
		)
	})
}

func (ui *ui) page(gtx layout.Context, title, subtitle string, back bool, content layout.Widget) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					brand := material.H6(ui.theme, "zka")
					brand.Color = ui.colors.accent
					return ui.selectableLabel(gtx, "page:brand", brand)
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
				return ui.selectableLabel(gtx, fmt.Sprintf("page:%d:title", ui.screen), heading)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 5, Bottom: 22}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(ui.theme, subtitle)
				label.Color = ui.colors.muted
				return ui.selectableLabel(gtx, fmt.Sprintf("page:%d:subtitle", ui.screen), label)
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

func (ui *ui) workspaceSectionHeader(gtx layout.Context, label string) layout.Dimensions {
	return layout.Inset{Top: 14, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		style := material.Subtitle2(ui.theme, label)
		style.Color = ui.colors.muted
		return ui.selectableLabel(gtx, "workspace-section:"+label, style)
	})
}

func (ui *ui) workspaceRow(gtx layout.Context, button, detachButton, credentialButton, forgetButton *widget.Clickable, credentialLabel string, workspace *zka.Workspace, action string, selected bool) layout.Dimensions {
	return layout.Inset{Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				key := "workspace:" + workspace.RemoteHost + ":" + workspace.ID
				remoteHost := workspace.RemoteHost
				if remoteHost == "" && ui.screen == screenRemoteList {
					remoteHost = ui.remoteHost
				}
				summary := workspaceSummary(workspace)
				if credentials := workspaceCredentialSummary(workspace, remoteHost, ui.localNodeID); credentials != "" {
					summary += "  ·  " + credentials
				}
				return ui.actionCard(gtx, key, button, workspace.Name, summary, action, selected)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if detachButton == nil && credentialButton == nil && forgetButton == nil {
					return layout.Dimensions{}
				}
				return layout.Inset{Left: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					children := make([]layout.FlexChild, 0, 3)
					if credentialButton != nil {
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return ui.credentialButton(gtx, credentialButton, credentialLabel)
						}))
					}
					if detachButton != nil {
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							inset := layout.Inset{}
							if credentialButton != nil {
								inset.Top = 6
							}
							return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return ui.detachButton(gtx, detachButton)
							})
						}))
					}
					if forgetButton != nil {
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							inset := layout.Inset{}
							if credentialButton != nil || detachButton != nil {
								inset.Top = 6
							}
							return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return ui.forgetButton(gtx, forgetButton)
							})
						}))
					}
					return layout.Flex{Axis: layout.Vertical, Alignment: layout.End}.Layout(gtx, children...)
				})
			}),
		)
	})
}

func (ui *ui) actionRow(gtx layout.Context, key string, button *widget.Clickable, title, subtitle string, selected bool) layout.Dimensions {
	return layout.Inset{Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return ui.actionCard(gtx, key, button, title, subtitle, "", selected)
	})
}

func (ui *ui) actionCard(gtx layout.Context, key string, button *widget.Clickable, title, subtitle, action string, selected bool) layout.Dimensions {
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
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							label := material.Subtitle1(ui.theme, title)
							if selected {
								label.Color = ui.colors.accent
							}
							return ui.selectableLabel(gtx, key+":title", label)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: 3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								label := material.Caption(ui.theme, subtitle)
								label.Color = ui.colors.muted
								return ui.selectableLabel(gtx, key+":subtitle", label)
							})
						}),
					)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if action == "" {
						return layout.Dimensions{}
					}
					return layout.Inset{Left: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						label := material.Body2(ui.theme, action)
						label.Color = ui.colors.accent
						return ui.selectableLabel(gtx, key+":action", label)
					})
				}),
			)
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

func (ui *ui) dangerButton(gtx layout.Context, button *widget.Clickable, label string) layout.Dimensions {
	style := material.Button(ui.theme, button, label)
	style.Background = ui.colors.danger
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

func (ui *ui) detachButton(gtx layout.Context, button *widget.Clickable) layout.Dimensions {
	style := material.Button(ui.theme, button, "Detach")
	style.Background = ui.colors.surface
	style.Color = ui.colors.danger
	style.CornerRadius = 9
	style.Inset = layout.Inset{Top: 10, Right: 12, Bottom: 10, Left: 12}
	return style.Layout(gtx)
}

func (ui *ui) forgetButton(gtx layout.Context, button *widget.Clickable) layout.Dimensions {
	style := material.Button(ui.theme, button, "Forget")
	style.Background = ui.colors.surface
	style.Color = ui.colors.danger
	style.CornerRadius = 9
	style.Inset = layout.Inset{Top: 10, Right: 12, Bottom: 10, Left: 12}
	return style.Layout(gtx)
}

func (ui *ui) credentialButton(gtx layout.Context, button *widget.Clickable, label string) layout.Dimensions {
	style := material.Button(ui.theme, button, label)
	style.Background = ui.colors.surface
	style.Color = ui.colors.accent
	style.CornerRadius = 9
	style.Inset = layout.Inset{Top: 10, Right: 12, Bottom: 10, Left: 12}
	return style.Layout(gtx)
}

func (ui *ui) operationMessage(gtx layout.Context) layout.Dimensions {
	if ui.errorMessage != "" {
		return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ui.message(gtx, fmt.Sprintf("screen:%d:operation", ui.screen), ui.errorMessage, true)
		})
	}
	if ui.status != "" {
		return layout.Inset{Top: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return ui.message(gtx, fmt.Sprintf("screen:%d:operation", ui.screen), ui.status, false)
		})
	}
	return layout.Dimensions{}
}

func (ui *ui) message(gtx layout.Context, key, message string, danger bool) layout.Dimensions {
	label := material.Body2(ui.theme, message)
	label.Color = ui.colors.muted
	if danger {
		label.Color = ui.colors.danger
	}
	return ui.selectableLabel(gtx, key, label)
}

func (ui *ui) centeredMessage(gtx layout.Context, message string) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return ui.message(gtx, fmt.Sprintf("screen:%d:centered-message", ui.screen), message, false)
	})
}

func (ui *ui) footer(gtx layout.Context) layout.Dimensions {
	text := "↑↓ Navigate    Enter Select    Esc Back"
	if ui.screen == screenHome {
		text = "↑↓ Navigate    Enter Switch/Attach    A Credentials    D Detach    F Forget    Esc Close"
	} else if ui.screen == screenRemoteList {
		text = "↑↓ Navigate    Enter Select    A Toggle credentials    Esc Back"
	}
	label := material.Caption(ui.theme, text)
	label.Color = ui.colors.muted
	return ui.selectableLabel(gtx, fmt.Sprintf("screen:%d:footer", ui.screen), label)
}
