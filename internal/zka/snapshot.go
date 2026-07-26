package zka

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type SessionTemplate struct {
	Lines    []templateLine
	Launches int
}

type templateLine struct {
	Line     sessionLine
	Launch   LaunchLine
	IsLaunch bool
}

var safeSessionDirectives = map[string]bool{
	"new_os_window": true, "os_window_state": true, "os_window_class": true,
	"os_window_name": true, "os_window_title": true, "os_window_size": true,
	"new_tab": true, "enabled_layouts": true,
	"layout": true, "set_layout_state": true, "focus": true,
	"focus_tab": true, "focus_os_window": true, "resize_window": true,
	"focus_matching_window": true, "cd": true, "title": true,
}

var launchValueOptions = map[string]bool{
	"--source-window": true, "--window-title": true, "--title": true,
	"--tab-title": true, "--cwd": true, "--add-to-session": true,
	"--location": true, "--next-to": true, "--bias": true, "--var": true,
	"--env": true, "--type": true, "--stdin-source": true,
	"--spacing": true, "--logo": true, "--logo-position": true,
	"--logo-alpha": true, "--color": true, "--watcher": true,
	"-w": true, "--marker": true, "--remote-control-password": true,
	"--os-window-class": true, "--os-window-name": true,
	"--os-window-title": true, "--os-window-state": true,
	"--os-window-position": true, "--os-panel": true,
}

var launchFlagOptions = map[string]bool{
	"--hold": true, "--keep-focus": true, "--dont-take-focus": true,
	"--copy-colors": true, "--copy-env": true, "--copy-cmdline": true,
	"--allow-remote-control": true, "--stdin-add-formatting": true,
	"--stdin-add-line-wrap-markers": true, "--hold-after-ssh": true,
}

var safeTopologyValueOptions = map[string]bool{
	"--window-title": true, "--title": true, "--tab-title": true,
	"--cwd": true, "--location": true, "--next-to": true, "--bias": true,
	"--var": true, "--os-window-class": true, "--os-window-name": true,
	"--os-window-title": true, "--os-window-state": true,
	"--os-window-position": true,
}

var safeTopologyFlagOptions = map[string]bool{
	"--keep-focus": true, "--dont-take-focus": true,
}

func DefaultSessionTemplate() SessionTemplate {
	return SessionTemplate{
		Lines:    []templateLine{{Line: sessionLine{Directive: "launch"}, IsLaunch: true}},
		Launches: 1,
	}
}

// ParseSessionTemplate stays strict: a user template is authored, not captured,
// so an unknown directive is a mistake worth reporting rather than something to
// tolerate.
func ParseSessionTemplate(content string) (SessionTemplate, error) {
	var template SessionTemplate
	for index, line := range parseSessionLines(content) {
		entry := templateLine{Line: line}
		if line.Directive == "launch" {
			launch, err := parseKittyLaunchLine(line.Rest)
			if err != nil {
				return SessionTemplate{}, fmt.Errorf("template line %d: %w", index+1, err)
			}
			if len(launch.Args) != 0 {
				return SessionTemplate{}, fmt.Errorf("template line %d: launch must not contain a program", index+1)
			}
			if err := validateTemplateOptions(launch.Options); err != nil {
				return SessionTemplate{}, fmt.Errorf("template line %d: %w", index+1, err)
			}
			entry.IsLaunch = true
			entry.Launch = launch
			template.Launches++
		} else if !safeSessionDirectives[line.Directive] {
			return SessionTemplate{}, fmt.Errorf("template line %d: directive %q is not topology-safe", index+1, line.Directive)
		}
		template.Lines = append(template.Lines, entry)
	}
	if template.Launches == 0 {
		return SessionTemplate{}, fmt.Errorf("template must contain at least one bare launch")
	}
	return template, nil
}

func validateTemplateOptions(options launchOptions) error {
	for _, option := range options {
		if !safeTopologyValueOptions[option.Name] && !safeTopologyFlagOptions[option.Name] {
			return fmt.Errorf("launch option %q is not topology-safe", option.Name)
		}
		if option.Name != "--var" && option.Name != "--env" {
			continue
		}
		key := option.Value
		if at := strings.IndexByte(key, '='); at >= 0 {
			key = key[:at]
		}
		if isManagedPaneVariable(key) {
			return fmt.Errorf("reserved variable %q is managed by zka", key)
		}
	}
	return nil
}

func optionParts(token string) (name, value string, hasValue bool) {
	if at := strings.IndexByte(token, '='); at >= 0 {
		return token[:at], token[at+1:], true
	}
	return token, "", false
}

func isManagedPaneVariable(key string) bool {
	return key == "zka_workspace" || key == "zka_pane" || key == "zka_state" || key == "zka_ready" ||
		key == "zka_os_window" || key == "zka_tab" || strings.HasPrefix(key, "ZKA_")
}

func GenerateManagedSession(template SessionTemplate, workspace *Workspace) (string, error) {
	panes := workspace.SortedPanes()
	if len(panes) != template.Launches {
		return "", fmt.Errorf("template has %d launches but workspace has %d panes", template.Launches, len(panes))
	}
	var out sessionWriter
	paneIndex := 0
	for _, line := range template.Lines {
		if !line.IsLaunch {
			reemitSessionLine(&out, line.Line, false)
			continue
		}
		pane := panes[paneIndex]
		pane.LaunchOptions = line.Launch.Options.clone()
		out.Launch(buildLaunch(launchSpec{Workspace: workspace, Pane: pane, Transport: Transport{Kind: "local"}}))
		paneIndex++
	}
	return out.String(), nil
}

// reemitSessionLine re-encodes a directive that was parsed from somewhere else.
// decoded says whether Rest came from zka's own rendering (already "$"-escaped)
// or from Kitty, which writes its values raw.
func reemitSessionLine(out *sessionWriter, line sessionLine, decoded bool) {
	value := line.Rest
	if decoded {
		value = line.verbatimValue()
	}
	switch line.Directive {
	case "new_os_window":
		out.NewOSWindow()
	case "focus":
		out.Focus()
	case "focus_os_window":
		out.FocusOSWindow()
	case "focus_tab":
		index, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || index < 0 {
			return
		}
		out.FocusTab(index)
	case "set_layout_state":
		// Never expanded by Kitty, so Rest is literal in both conventions.
		out.LayoutState([]byte(line.Rest))
	case "enabled_layouts":
		out.EnabledLayouts(strings.Split(value, ","))
	case "layout":
		out.Layout(value)
	case "new_tab":
		out.NewTab(stripStateMarker(value))
	case "title":
		out.Title(stripStateMarker(value))
	case "cd":
		out.CD(value)
	case "os_window_state":
		out.OSWindowState(value)
	case "os_window_class":
		out.OSWindowClass(value)
	case "os_window_name":
		out.OSWindowName(value)
	case "os_window_title":
		out.OSWindowTitle(value)
	case "os_window_size":
		fields := strings.Fields(value)
		if len(fields) == 2 {
			out.OSWindowSize(fields[0], fields[1])
		}
	case "resize_window":
		out.ResizeWindow(strings.Fields(value))
	case "focus_matching_window":
		out.FocusMatching(value)
	}
}

// CanonicalizeKittySession rewrites Kitty's own session output into zka's
// canonical form. Directives Kitty wrote are raw, so they are re-encoded; every
// launch is rebuilt from scratch through buildLaunch. Unknown directives are
// dropped with the rest of the capture intact -- capture must never fail
// because Kitty grew a directive zka has not seen.
func CanonicalizeKittySession(content string, workspace *Workspace) (string, error) {
	var out sessionWriter
	seen := map[string]bool{}
	for index, line := range parseSessionLines(content) {
		if line.Directive != "launch" {
			if !safeSessionDirectives[line.Directive] {
				continue
			}
			if workspace.RemoteHost != "" && line.Directive == "cd" {
				continue
			}
			reemitSessionLine(&out, line, false)
			continue
		}
		launch, err := parseKittyLaunchLine(line.Rest)
		if err != nil {
			return "", fmt.Errorf("kitty session line %d: %w", index+1, err)
		}
		workspaceID := launch.Options.VarValue("--var", "zka_workspace")
		paneID := launch.Options.VarValue("--var", "zka_pane")
		if workspaceID != workspace.ID {
			return "", fmt.Errorf("kitty session line %d: launch is not tagged for workspace %s", index+1, workspace.ID)
		}
		pane := workspace.Panes[paneID]
		if pane == nil {
			return "", fmt.Errorf("kitty session line %d: unknown pane %q", index+1, paneID)
		}
		if seen[paneID] {
			return "", fmt.Errorf("kitty session line %d: pane %s is duplicated", index+1, paneID)
		}
		seen[paneID] = true
		replay := pane.Clone()
		replay.LaunchOptions = launch.Options.clone()
		if workspace.RemoteHost != "" {
			replay.LaunchOptions = replay.LaunchOptions.Drop("--cwd")
		}
		out.Launch(buildLaunch(launchSpec{Workspace: workspace, Pane: replay, Transport: Transport{Kind: "local"}}))
	}
	if len(seen) == 0 {
		return "", fmt.Errorf("kitty session contains no managed panes")
	}
	return out.String(), nil
}

func RenderAttachmentSession(workspace *Workspace, transport Transport, attachmentID string) (string, error) {
	if len(workspace.Topology.Roots) != 0 {
		return renderDesiredTopologySession(workspace, transport, attachmentID)
	}
	if strings.TrimSpace(workspace.Manifest.Session) == "" {
		return "", fmt.Errorf("workspace %s has no captured manifest", workspace.Name)
	}
	var out sessionWriter
	seen := map[string]bool{}
	for index, line := range parseSessionLines(workspace.Manifest.Session) {
		if line.Directive != "launch" {
			if transport.Kind == "ssh" && line.Directive == "cd" {
				continue
			}
			reemitSessionLine(&out, line, true)
			continue
		}
		launch, err := parseZkaLaunchLine(line.Rest)
		if err != nil {
			return "", fmt.Errorf("manifest line %d: %w", index+1, err)
		}
		paneID := launch.Options.VarValue("--var", "zka_pane")
		pane := workspace.Panes[paneID]
		if pane == nil {
			return "", fmt.Errorf("manifest references unknown pane %s", paneID)
		}
		if seen[paneID] {
			return "", fmt.Errorf("manifest duplicates pane %s", paneID)
		}
		seen[paneID] = true
		replay := pane.Clone()
		replay.LaunchOptions = launch.Options.clone()
		out.Launch(buildLaunch(launchSpec{
			Workspace: workspace, Pane: replay, Transport: transport, AttachmentID: attachmentID,
		}))
	}
	return out.String(), nil
}

func renderDesiredTopologySession(workspace *Workspace, transport Transport, attachmentID string) (string, error) {
	if err := validateTopology(workspace, workspace.Topology.Roots, activeTopologyPaneIDs(workspace)); err != nil {
		return "", fmt.Errorf("invalid desired topology: %w", err)
	}
	var out sessionWriter
	focusPane := workspace.RestoreFocusPaneID
	if focusPane == "" {
		for _, osNode := range workspace.Topology.Roots {
			for _, tabNode := range osNode.Children {
				if len(tabNode.Children) != 0 {
					focusPane = tabNode.Children[0].PaneID
					break
				}
			}
			if focusPane != "" {
				break
			}
		}
	}
	for osIndex, osNode := range workspace.Topology.Roots {
		if osIndex > 0 {
			out.NewOSWindow()
		}
		out.OSWindowState(osNode.State)
		out.OSWindowClass(osNode.Class)
		out.OSWindowName(osNode.Name)
		focusTabIndex := -1
		for tabIndex, tabNode := range osNode.Children {
			out.NewTab(tabNode.Title)
			// enabled_layouts must precede layout: set_enabled_layouts resets
			// the current layout when it is not in the new list.
			out.EnabledLayouts(tabNode.EnabledLayouts)
			out.Layout(tabNode.Layout)
			out.LayoutState(tabNode.LayoutState)
			for paneIndex, paneNode := range tabNode.Children {
				pane := workspace.Panes[paneNode.PaneID]
				if pane == nil {
					return "", fmt.Errorf("desired topology references unknown pane %s", paneNode.PaneID)
				}
				serializedWindowID := int64(0)
				if len(tabNode.LayoutState) != 0 {
					serializedWindowID = int64(paneIndex + 1)
				}
				out.Launch(buildLaunch(launchSpec{
					Workspace: workspace, Pane: pane, Transport: transport, AttachmentID: attachmentID,
					OSWindowNodeID: osNode.ID, TabNodeID: tabNode.ID, SerializedWindowID: serializedWindowID,
				}))
				if pane.ID == focusPane {
					out.Focus()
					focusTabIndex = tabIndex
				}
			}
		}
		// Kitty yields and replaces its session object at every new_os_window,
		// so focus_tab is per-OS-window and focus_os_window must sit inside the
		// block whose window should end up focused.
		if focusTabIndex >= 0 {
			out.FocusTab(focusTabIndex)
			out.FocusOSWindow()
		}
	}
	return out.String(), nil
}

// applyManifestLaunchOptions refreshes each pane's replayable launch options
// from a canonical manifest. It reports parse failures instead of silently
// dropping options, which previously hid a whole class of capture damage.
func applyManifestLaunchOptions(workspace *Workspace, session string) error {
	for index, line := range parseSessionLines(session) {
		if line.Directive != "launch" {
			continue
		}
		launch, err := parseZkaLaunchLine(line.Rest)
		if err != nil {
			return fmt.Errorf("manifest line %d: %w", index+1, err)
		}
		pane := workspace.Panes[launch.Options.VarValue("--var", "zka_pane")]
		if pane == nil {
			continue
		}
		// cwd and titles are derived from the pane model on every render, so
		// keeping a captured copy here would just be a second source of truth.
		pane.LaunchOptions = stripManagedOptions(launch.Options).
			Drop("--cwd", "--title", "--window-title", "--tab-title")
	}
	return nil
}

func CaptureManifest(ctx context.Context, kitty KittyClient, endpoint string, workspace *Workspace) (Manifest, map[string]RuntimeView, error) {
	tree, err := kitty.List(ctx, endpoint)
	if err != nil {
		return Manifest{}, nil, err
	}
	views, untagged := findWorkspaceViews(tree, workspace.ID)
	if foreign := foreignUntaggedWindows(untagged); len(foreign) > 0 {
		return Manifest{}, nil, fmt.Errorf("%w: %v", errKittyNotQuiescent, foreign)
	}
	topology, err := topologyFromKitty(tree, workspace.ID)
	if err != nil {
		return Manifest{}, nil, err
	}
	native, err := kitty.NativeSession(ctx, endpoint)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("capture kitty session: %w", err)
	}
	canonical, err := CanonicalizeKittySession(native, workspace)
	if err != nil {
		return Manifest{}, nil, err
	}
	manifest := Manifest{KittyVersion: kitty.Version(ctx), CapturedAt: timeNowUTC(), Session: canonical, Topology: topology}
	if err := validateManifest(workspace, manifest); err != nil {
		return Manifest{}, nil, err
	}
	return manifest, views, nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }
