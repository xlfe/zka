package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type KittyClient struct {
	Runner  CommandRunner
	Command string
}

type kittyOSWindow struct {
	ID        int64      `json:"id"`
	IsFocused bool       `json:"is_focused"`
	State     string     `json:"state"`
	WMClass   string     `json:"wm_class"`
	WMName    string     `json:"wm_name"`
	Tabs      []kittyTab `json:"tabs"`
}

type kittyTab struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	Layout      string          `json:"layout"`
	Enabled     []string        `json:"enabled_layouts"`
	LayoutState json.RawMessage `json:"layout_state"`
	IsFocused   bool            `json:"is_focused"`
	IsActive    bool            `json:"is_active"`
	Windows     []kittyWindow   `json:"windows"`

	// TitleOverridden distinguishes a tab that was explicitly named from one
	// reporting its active window's live title. Kitty's `ls` sends
	// "title": tab.name or tab.title, so without this a transient program
	// title gets captured and then pinned as a permanent tab name. A nil
	// pointer means the field was absent, i.e. a Kitty older than 0.47.
	TitleOverridden *bool `json:"title_overridden"`
}

// namedTitle returns the tab's explicit name, or "" when the tab is unnamed and
// is merely echoing its active window's title.
func (t kittyTab) namedTitle() string {
	if t.TitleOverridden != nil && !*t.TitleOverridden {
		return ""
	}
	return canonicalStrippedValue(stripStateMarker(t.Title))
}

func (t kittyTab) namedTitleKnown() bool { return t.TitleOverridden != nil }

type kittyWindow struct {
	ID        int64             `json:"id"`
	Title     string            `json:"title"`
	CWD       string            `json:"cwd"`
	IsFocused bool              `json:"is_focused"`
	IsActive  bool              `json:"is_active"`
	UserVars  map[string]string `json:"user_vars"`
	Env       map[string]string `json:"env"`
	Cmdline   []string          `json:"cmdline"`
}

func (k KittyClient) command() string {
	if k.Command != "" {
		return k.Command
	}
	return "kitten"
}

func (k KittyClient) rc(ctx context.Context, endpoint string, args ...string) (string, error) {
	all := []string{"@"}
	if endpoint != "" {
		all = append(all, "--to", endpoint)
	}
	all = append(all, args...)
	out, _, err := k.Runner.Run(ctx, k.command(), all...)
	return out, err
}

func (k KittyClient) List(ctx context.Context, endpoint string) ([]kittyOSWindow, error) {
	out, err := k.rc(ctx, endpoint, "ls")
	if err != nil {
		return nil, err
	}
	var windows []kittyOSWindow
	if err := json.Unmarshal([]byte(out), &windows); err != nil {
		return nil, fmt.Errorf("decode kitty window tree: %w", err)
	}
	for oi := range windows {
		for ti := range windows[oi].Tabs {
			for wi := range windows[oi].Tabs[ti].Windows {
				window := &windows[oi].Tabs[ti].Windows[wi]
				if window.UserVars == nil {
					window.UserVars = map[string]string{}
				}
			}
		}
	}
	return windows, nil
}

func (k KittyClient) NativeSession(ctx context.Context, endpoint string) (string, error) {
	// Each attachment owns a dedicated Kitty process. Kitty 0.47 returns JSON
	// when --match is combined with --output-format=session, so capture the
	// entire process and let CaptureManifest validate its workspace tags.
	return k.rc(ctx, endpoint, "ls", "--output-format=session")
}

func (k KittyClient) Version(ctx context.Context) string {
	out, _, err := k.Runner.Run(ctx, k.command(), "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (k KittyClient) FocusWorkspace(ctx context.Context, endpoint, workspaceID string) error {
	_, err := k.rc(ctx, endpoint, "focus-window", "--match", "var:zka_workspace="+workspaceID)
	return err
}

func (k KittyClient) FocusPane(ctx context.Context, endpoint, workspaceID, paneID string) error {
	match := "var:zka_workspace=" + workspaceID
	if paneID != "" {
		match = "var:zka_pane=" + paneID
	}
	_, err := k.rc(ctx, endpoint, "focus-window", "--match", match)
	return err
}

func (k KittyClient) CloseWorkspace(ctx context.Context, endpoint, workspaceID string) error {
	_, err := k.rc(ctx, endpoint, "close-window", "--no-response", "--match", "var:zka_workspace="+workspaceID)
	return err
}

func (k KittyClient) CloseWindow(ctx context.Context, endpoint string, windowID int64) error {
	if windowID <= 0 {
		return fmt.Errorf("cannot close invalid Kitty window id %d", windowID)
	}
	_, err := k.rc(ctx, endpoint, "close-window", "--match", "id:"+strconv.FormatInt(windowID, 10))
	return err
}

func (k KittyClient) DetachWindow(ctx context.Context, endpoint string, windowID int64, targetTabID int64) error {
	args := []string{"detach-window", "--match", "id:" + strconv.FormatInt(windowID, 10)}
	if targetTabID > 0 {
		args = append(args, "--target-tab", "id:"+strconv.FormatInt(targetTabID, 10))
	} else {
		args = append(args, "--target-tab", "new")
	}
	_, err := k.rc(ctx, endpoint, args...)
	return err
}

func (k KittyClient) DetachTab(ctx context.Context, endpoint string, tabID, targetTabID int64) error {
	args := []string{"detach-tab", "--match", "id:" + strconv.FormatInt(tabID, 10)}
	if targetTabID > 0 {
		args = append(args, "--target-tab", "id:"+strconv.FormatInt(targetTabID, 10))
	}
	_, err := k.rc(ctx, endpoint, args...)
	return err
}

func (k KittyClient) LoadSession(ctx context.Context, endpoint, path string, anchorWindowID int64) error {
	if path == "" {
		return fmt.Errorf("cannot load an empty Kitty session path")
	}
	if anchorWindowID <= 0 {
		return fmt.Errorf("cannot load a Kitty session without an anchor window")
	}
	_, err := k.rc(ctx, endpoint, "action", "--match", "id:"+strconv.FormatInt(anchorWindowID, 10), "goto_session", path)
	return err
}

func (k KittyClient) SetEnabledLayouts(ctx context.Context, endpoint string, tabID int64, enabled []string) error {
	if len(enabled) == 0 {
		return nil
	}
	args := []string{"set-enabled-layouts", "--match", "id:" + strconv.FormatInt(tabID, 10)}
	args = append(args, enabled...)
	_, err := k.rc(ctx, endpoint, args...)
	return err
}

func (k KittyClient) GotoLayout(ctx context.Context, endpoint string, tabID int64, layout string) error {
	if layout == "" {
		return nil
	}
	_, err := k.rc(ctx, endpoint, "goto-layout", "--match", "id:"+strconv.FormatInt(tabID, 10), layout)
	return err
}

func (k KittyClient) LaunchPane(ctx context.Context, endpoint string, workspace *Workspace, pane *Pane, transport Transport, attachmentID, osWindowNodeID, tabNodeID, launchType string, anchorWindowID int64) (int64, error) {
	if pane == nil {
		return 0, fmt.Errorf("cannot launch an empty pane")
	}
	if anchorWindowID > 0 {
		if _, err := k.rc(ctx, endpoint, "focus-window", "--match", "id:"+strconv.FormatInt(anchorWindowID, 10)); err != nil {
			return 0, err
		}
	}
	line := buildLaunch(launchSpec{
		Workspace: workspace, Pane: pane, Transport: transport, AttachmentID: attachmentID,
		OSWindowNodeID: osWindowNodeID, TabNodeID: tabNodeID,
	})
	out, err := k.rc(ctx, endpoint, line.RCArgs(launchType, anchorWindowID, true)...)
	if err != nil {
		return 0, err
	}
	windowID, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || windowID <= 0 {
		return 0, fmt.Errorf("decode launched Kitty window id %q", strings.TrimSpace(out))
	}
	return windowID, nil
}

func (k KittyClient) SetPaneState(ctx context.Context, endpoint string, view RuntimeView, workspace *Workspace, pane *Pane) error {
	match := "id:" + strconv.FormatInt(view.WindowID, 10)
	if _, err := k.rc(ctx, endpoint, "set-user-vars", "--match", match,
		"zka_workspace="+workspace.ID, "zka_pane="+pane.ID, "zka_state="+string(pane.State)); err != nil {
		return err
	}
	title := strings.TrimSpace(stateMarker(pane.State) + " " + pane.Title)
	// Keep the title child-controlled. Without --temporary Kitty installs a
	// permanent override, so a shell can no longer update it after chdir.
	// kitten expands ANSI-C escapes in positional arguments and refuses one
	// that looks like an option, so the title is escaped and placed after "--".
	_, err := k.rc(ctx, endpoint, "set-window-title", "--temporary", "--match", match, "--", ansiCEscape(title))
	return err
}

// PaneForWindow maps a Kitty window id to the zka pane tagged on it. It
// returns "" for an untagged window, a window belonging to another workspace,
// or any failure: a missing source pane must degrade to "no hint", never fail
// the launch that asked for it.
func (k KittyClient) PaneForWindow(ctx context.Context, endpoint, workspaceID string, windowID int64) string {
	if endpoint == "" || windowID <= 0 {
		return ""
	}
	tree, err := k.List(ctx, endpoint)
	if err != nil {
		return ""
	}
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				if window.ID != windowID {
					continue
				}
				if window.UserVars["zka_workspace"] != workspaceID {
					return ""
				}
				return window.UserVars["zka_pane"]
			}
		}
	}
	return ""
}

func (k KittyClient) SetIdentity(ctx context.Context, endpoint string, windowID int64, workspaceID, paneID string) error {
	if endpoint == "" || windowID <= 0 {
		return fmt.Errorf("current Kitty window identity is unavailable")
	}
	_, err := k.rc(ctx, endpoint, "set-user-vars", "--match", "id:"+strconv.FormatInt(windowID, 10),
		"zka_workspace="+workspaceID, "zka_pane="+paneID, "zka_state="+string(StateUnknown), "zka_ready=0")
	return err
}

func (k KittyClient) SetPaneReady(ctx context.Context, endpoint string, windowID int64, ready bool) error {
	if endpoint == "" || windowID <= 0 {
		return fmt.Errorf("current Kitty window identity is unavailable")
	}
	value := "0"
	if ready {
		value = "1"
	}
	_, err := k.rc(ctx, endpoint, "set-user-vars", "--match", "id:"+strconv.FormatInt(windowID, 10), "zka_ready="+value)
	return err
}

func (k KittyClient) SetTopologyIdentity(ctx context.Context, endpoint string, windowID int64, tabNodeID, osWindowNodeID string) error {
	if endpoint == "" || windowID <= 0 || tabNodeID == "" || osWindowNodeID == "" {
		return fmt.Errorf("current Kitty topology identity is unavailable")
	}
	_, err := k.rc(ctx, endpoint, "set-user-vars", "--match", "id:"+strconv.FormatInt(windowID, 10),
		"zka_tab="+tabNodeID, "zka_os_window="+osWindowNodeID)
	return err
}

// SetTabTitle names a tab, or clears the name when title is empty so the tab
// falls back to reporting its active window's title. Like set-window-title,
// kitten expands ANSI-C escapes in the positional argument.
func (k KittyClient) SetTabTitle(ctx context.Context, endpoint string, tabID int64, title string) error {
	args := []string{"set-tab-title", "--match", "id:" + strconv.FormatInt(tabID, 10)}
	if title != "" {
		args = append(args, "--", ansiCEscape(title))
	}
	_, err := k.rc(ctx, endpoint, args...)
	return err
}

// Desktop notifications deliberately do not live here. They used to run
// `kitty @ run kitten notify --wait-for-completion`, which could never work:
// kitty @ run gives the child no controlling terminal, and that kitten writes
// an OSC 99 escape sequence to a tty. See DesktopNotifier in desktop.go.

// untaggedWindow is a Kitty window carrying no zka identity. Nascent ones are
// managed panes whose `zka pane` process has not called SetIdentity yet; they
// are a normal, momentary state and must not fail the whole capture the way a
// genuinely foreign window does.
type untaggedWindow struct {
	ID      int64
	Nascent bool
}

func foreignUntaggedWindows(windows []untaggedWindow) []int64 {
	var foreign []int64
	for _, window := range windows {
		if !window.Nascent {
			foreign = append(foreign, window.ID)
		}
	}
	return foreign
}

// nascentManagedWindow reports whether an untagged window is a managed shell
// that is still starting up. The managed Kitty runs `zka pane --workspace X`
// as its shell, so the cmdline identifies it from creation.
func nascentManagedWindow(window kittyWindow, workspaceID string) bool {
	if len(window.Cmdline) < 2 {
		return false
	}
	base := window.Cmdline[0]
	if at := strings.LastIndexByte(base, '/'); at >= 0 {
		base = base[at+1:]
	}
	if base != "zka" {
		return false
	}
	switch window.Cmdline[1] {
	case "pane", "remote-pane", "remote-new-pane":
	default:
		return false
	}
	if window.Env["ZKA_WORKSPACE_ID"] != "" && window.Env["ZKA_WORKSPACE_ID"] != workspaceID {
		return false
	}
	for index, token := range window.Cmdline {
		if token == "--workspace" && index+1 < len(window.Cmdline) {
			return window.Cmdline[index+1] == workspaceID
		}
	}
	return true
}

func findWorkspaceViews(tree []kittyOSWindow, workspaceID string) (map[string]RuntimeView, []untaggedWindow) {
	result := map[string]RuntimeView{}
	var untagged []untaggedWindow
	now := time.Now().UTC()
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				workspace := window.UserVars["zka_workspace"]
				pane := window.UserVars["zka_pane"]
				if workspace == "" || pane == "" {
					untagged = append(untagged, untaggedWindow{
						ID: window.ID, Nascent: nascentManagedWindow(window, workspaceID),
					})
					continue
				}
				if workspace != workspaceID {
					continue
				}
				result[pane] = RuntimeView{
					PaneID: pane, WindowID: window.ID, TabID: tab.ID, OSWindowID: osWindow.ID,
					TabNodeID: window.UserVars["zka_tab"], OSWindowNodeID: window.UserVars["zka_os_window"],
					Focused: window.IsFocused || (tab.IsFocused && window.IsActive) || (osWindow.IsFocused && tab.IsActive && window.IsActive),
					Ready:   window.UserVars["zka_ready"] == "1", LastSeen: now,
				}
			}
		}
	}
	return result, untagged
}

func topologyFromKitty(tree []kittyOSWindow, workspaceID string) ([]Node, error) {
	var topology []Node
	for _, osWindow := range tree {
		osNode := Node{Kind: "os-window", State: osWindow.State,
			Class: canonicalStrippedValue(osWindow.WMClass), Name: canonicalStrippedValue(osWindow.WMName),
			Focused: osWindow.IsFocused}
		osIdentityConflict := false
		for _, tab := range osWindow.Tabs {
			layoutState, err := logicalKittyLayoutState(tab)
			if err != nil {
				return nil, fmt.Errorf("normalize Kitty tab %d layout state: %w", tab.ID, err)
			}
			titleKnown := tab.namedTitleKnown()
			tabNode := Node{Kind: "tab", Title: tab.namedTitle(), Layout: tab.Layout, EnabledLayouts: append([]string(nil), tab.Enabled...), LayoutState: layoutState, Active: tab.IsActive, Focused: tab.IsFocused, TitleKnown: &titleKnown}
			tabIdentityConflict := false
			for _, window := range tab.Windows {
				paneID := window.UserVars["zka_pane"]
				if window.UserVars["zka_workspace"] == "" || paneID == "" {
					// A managed pane that has not tagged itself yet. It is not
					// part of the topology, and it is not an error either.
					if nascentManagedWindow(window, workspaceID) {
						continue
					}
					return nil, fmt.Errorf("%w: kitty window %d", errKittyNotQuiescent, window.ID)
				}
				if window.UserVars["zka_workspace"] != workspaceID {
					return nil, fmt.Errorf("kitty window %d is not tagged for workspace %s", window.ID, workspaceID)
				}
				tabID := window.UserVars["zka_tab"]
				osWindowID := window.UserVars["zka_os_window"]
				if tabNode.ID == "" {
					tabNode.ID = tabID
				} else if tabID != "" && tabNode.ID != tabID {
					tabIdentityConflict = true
				}
				if osNode.ID == "" {
					osNode.ID = osWindowID
				} else if osWindowID != "" && osNode.ID != osWindowID {
					osIdentityConflict = true
				}
				tabNode.Children = append(tabNode.Children, Node{Kind: "pane", PaneID: paneID,
					Title: canonicalStrippedValue(stripStateMarker(window.Title)), CWD: window.CWD,
					Active: window.IsActive, Focused: window.IsFocused})
			}
			if tabIdentityConflict {
				tabNode.ID = ""
			}
			if len(tabNode.Children) > 0 {
				osNode.Children = append(osNode.Children, tabNode)
			}
		}
		if osIdentityConflict {
			osNode.ID = ""
		}
		if len(osNode.Children) > 0 {
			topology = append(topology, osNode)
		}
	}
	if len(topology) == 0 {
		return nil, fmt.Errorf("kitty instance has no panes for workspace %s", workspaceID)
	}
	return topology, nil
}

// logicalKittyLayoutState rewrites Kitty's per-process window and group IDs to
// deterministic tab-local IDs. The resulting opaque state can be compared
// across attachments and fed back to a Kitty session using matching serialized
// launch IDs.
func logicalKittyLayoutState(tab kittyTab) (json.RawMessage, error) {
	if len(tab.LayoutState) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(tab.LayoutState))
	decoder.UseNumber()
	var state map[string]any
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode layout state: %w", err)
	}
	allWindows, ok := state["all_windows"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("layout state has no all_windows object")
	}
	rawGroups, ok := allWindows["window_groups"].([]any)
	if !ok {
		return nil, fmt.Errorf("layout state has no window_groups array")
	}
	logicalWindowIDs := make(map[int64]int64, len(tab.Windows))
	for index, window := range tab.Windows {
		logicalWindowIDs[window.ID] = int64(index + 1)
	}
	groupIDs := make(map[int64]int64, len(rawGroups))
	groups := make([]any, 0, len(rawGroups))
	for _, rawGroup := range rawGroups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("layout state contains a non-object window group")
		}
		oldGroupID, err := layoutStateInt64(group["id"])
		if err != nil {
			return nil, fmt.Errorf("decode layout group id: %w", err)
		}
		rawWindowIDs, ok := group["window_ids"].([]any)
		if !ok {
			return nil, fmt.Errorf("layout group %d has no window_ids array", oldGroupID)
		}
		windowIDs := make([]any, 0, len(rawWindowIDs))
		var logicalGroupID int64
		for _, rawWindowID := range rawWindowIDs {
			oldWindowID, err := layoutStateInt64(rawWindowID)
			if err != nil {
				return nil, fmt.Errorf("decode layout window id: %w", err)
			}
			logicalWindowID, exists := logicalWindowIDs[oldWindowID]
			if !exists {
				continue
			}
			if logicalGroupID == 0 {
				logicalGroupID = logicalWindowID
			}
			windowIDs = append(windowIDs, logicalWindowID)
		}
		if logicalGroupID == 0 {
			continue
		}
		groupIDs[oldGroupID] = logicalGroupID
		groups = append(groups, map[string]any{
			"id": logicalGroupID, "window_ids": windowIDs,
		})
	}
	if len(groups) != len(tab.Windows) {
		return nil, fmt.Errorf("layout state maps %d window groups to %d managed panes", len(groups), len(tab.Windows))
	}
	state["all_windows"] = map[string]any{
		"active_group_idx":     0,
		"active_group_history": []any{},
		"window_groups":        groups,
	}
	if pairs, exists := state["pairs"]; exists {
		normalized, err := remapLayoutPair(pairs, groupIDs)
		if err != nil {
			return nil, err
		}
		state["pairs"] = normalized
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode logical layout state: %w", err)
	}
	return encoded, nil
}

func layoutStateInt64(value any) (int64, error) {
	switch value := value.(type) {
	case json.Number:
		return value.Int64()
	case float64:
		return int64(value), nil
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", value)
	}
}

func remapLayoutPair(value any, groupIDs map[int64]int64) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			if key == "one" || key == "two" {
				remapped, err := remapLayoutPair(child, groupIDs)
				if err != nil {
					return nil, err
				}
				result[key] = remapped
			} else {
				result[key] = child
			}
		}
		return result, nil
	case json.Number, float64, int64, int:
		oldGroupID, err := layoutStateInt64(value)
		if err != nil {
			return nil, err
		}
		logicalGroupID, exists := groupIDs[oldGroupID]
		if !exists {
			return nil, fmt.Errorf("split layout references unknown group %d", oldGroupID)
		}
		return logicalGroupID, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("split layout contains unsupported pair value %T", value)
	}
}
