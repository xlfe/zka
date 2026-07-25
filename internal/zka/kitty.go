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
}

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

func (k KittyClient) SetTabLayout(ctx context.Context, endpoint string, tabID int64, layout string, enabled []string) error {
	match := "id:" + strconv.FormatInt(tabID, 10)
	if len(enabled) != 0 {
		args := []string{"set-enabled-layouts", "--match", match}
		args = append(args, enabled...)
		if _, err := k.rc(ctx, endpoint, args...); err != nil {
			return err
		}
	}
	if layout != "" {
		if _, err := k.rc(ctx, endpoint, "goto-layout", "--match", match, layout); err != nil {
			return err
		}
	}
	return nil
}

func (k KittyClient) LaunchPane(ctx context.Context, endpoint string, workspace *Workspace, pane *Pane, transport Transport, attachmentID, osWindowNodeID, tabNodeID, launchType string, anchorWindowID int64) (int64, error) {
	if pane == nil {
		return 0, fmt.Errorf("cannot launch an empty pane")
	}
	if launchType == "" {
		launchType = "window"
	}
	if anchorWindowID > 0 {
		if _, err := k.rc(ctx, endpoint, "focus-window", "--match", "id:"+strconv.FormatInt(anchorWindowID, 10)); err != nil {
			return 0, err
		}
	}
	options := stripManagedOptions(pane.LaunchOptions)
	if transport.Kind == "ssh" {
		options = dropLaunchOption(options, "--cwd")
	}
	args := []string{"launch", "--type=" + launchType, "--dont-take-focus"}
	args = append(args, options...)
	if anchorWindowID > 0 {
		args = append(args, "--next-to", "id:"+strconv.FormatInt(anchorWindowID, 10))
	}
	if transport.Kind != "ssh" && !hasLaunchOption(options, "--cwd") && pane.CWD != "" {
		args = append(args, "--cwd", pane.CWD)
	}
	if !hasLaunchOption(options, "--title") && !hasLaunchOption(options, "--window-title") && pane.Title != "" {
		args = append(args, "--title", pane.Title)
	}
	args = append(args,
		"--var", "zka_workspace="+workspace.ID,
		"--var", "zka_pane="+pane.ID,
		"--var", "zka_state="+string(pane.State),
		"--var", "zka_ready=0",
		"--var", "zka_os_window="+osWindowNodeID,
		"--var", "zka_tab="+tabNodeID,
		"--env", "ZKA_WORKSPACE_ID="+workspace.ID,
		"--env", "ZKA_PANE_ID="+pane.ID,
	)
	if transport.Kind == "ssh" {
		args = append(args, "zka", "remote-pane", "--origin", transport.Host,
			"--workspace", workspace.ID, "--pane", pane.ID, "--attachment", attachmentID)
	} else {
		args = append(args, "zka", "pane", "--workspace", workspace.ID, "--pane", pane.ID)
	}
	out, err := k.rc(ctx, endpoint, args...)
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
	_, err := k.rc(ctx, endpoint, "set-window-title", "--match", match, title)
	return err
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

func (k KittyClient) SetTabTitle(ctx context.Context, endpoint string, tabID int64, title string) error {
	_, err := k.rc(ctx, endpoint, "set-tab-title", "--match", "id:"+strconv.FormatInt(tabID, 10), title)
	return err
}

func (k KittyClient) Notify(ctx context.Context, view RuntimeView, endpoint string, workspace *Workspace, pane *Pane) (string, error) {
	urgency, icon := "normal", "info"
	switch pane.State {
	case StateBlocked:
		urgency, icon = "critical", "question"
	case StateError:
		urgency, icon = "critical", "error"
	}
	identifier := "zka-" + workspace.ID + "-" + pane.ID
	callCtx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	return k.rc(callCtx, endpoint, "run", k.command(), "notify",
		"--app-name", "zka", "--identifier", identifier,
		"--urgency", urgency, "--icon", icon,
		"--button", "Focus", "--wait-for-completion",
		notificationTitle(workspace, pane), notificationBody(workspace, pane, true))
}

func (k KittyClient) CloseNotification(ctx context.Context, endpoint, workspaceID, paneID string) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = k.rc(callCtx, endpoint, "run", k.command(), "notify", "--identifier", "zka-"+workspaceID+"-"+paneID)
}

func findWorkspaceViews(tree []kittyOSWindow, workspaceID string) (map[string]RuntimeView, []int64) {
	result := map[string]RuntimeView{}
	var untagged []int64
	now := time.Now().UTC()
	for _, osWindow := range tree {
		for _, tab := range osWindow.Tabs {
			for _, window := range tab.Windows {
				workspace := window.UserVars["zka_workspace"]
				pane := window.UserVars["zka_pane"]
				if workspace == "" || pane == "" {
					untagged = append(untagged, window.ID)
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
		osNode := Node{Kind: "os-window", State: osWindow.State, Class: osWindow.WMClass, Name: osWindow.WMName, Focused: osWindow.IsFocused}
		osIdentityConflict := false
		for _, tab := range osWindow.Tabs {
			layoutState, err := logicalKittyLayoutState(tab)
			if err != nil {
				return nil, fmt.Errorf("normalize Kitty tab %d layout state: %w", tab.ID, err)
			}
			tabNode := Node{Kind: "tab", Title: stripStateMarker(tab.Title), Layout: tab.Layout, EnabledLayouts: append([]string(nil), tab.Enabled...), LayoutState: layoutState, Active: tab.IsActive, Focused: tab.IsFocused}
			tabIdentityConflict := false
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] != workspaceID {
					return nil, fmt.Errorf("kitty window %d is not tagged for workspace %s", window.ID, workspaceID)
				}
				paneID := window.UserVars["zka_pane"]
				if paneID == "" {
					return nil, fmt.Errorf("kitty window %d has no zka_pane tag", window.ID)
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
				tabNode.Children = append(tabNode.Children, Node{Kind: "pane", PaneID: paneID, Title: stripStateMarker(window.Title), CWD: window.CWD, Active: window.IsActive, Focused: window.IsFocused})
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

func quoteKitty(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "$", "$$")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return "\"" + value + "\""
}
