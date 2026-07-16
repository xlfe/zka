package zka

import (
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
	_, err := k.rc(ctx, endpoint, "close-window", "--match", "var:zka_workspace="+workspaceID)
	return err
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
		notificationTitle(workspace, pane), notificationBody(workspace, pane))
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
		for _, tab := range osWindow.Tabs {
			tabNode := Node{Kind: "tab", Title: stripStateMarker(tab.Title), Layout: tab.Layout, EnabledLayouts: append([]string(nil), tab.Enabled...), LayoutState: append(json.RawMessage(nil), tab.LayoutState...), Active: tab.IsActive, Focused: tab.IsFocused}
			for _, window := range tab.Windows {
				if window.UserVars["zka_workspace"] != workspaceID {
					return nil, fmt.Errorf("kitty window %d is not tagged for workspace %s", window.ID, workspaceID)
				}
				paneID := window.UserVars["zka_pane"]
				if paneID == "" {
					return nil, fmt.Errorf("kitty window %d has no zka_pane tag", window.ID)
				}
				tabNode.Children = append(tabNode.Children, Node{Kind: "pane", PaneID: paneID, Title: stripStateMarker(window.Title), CWD: window.CWD, Active: window.IsActive, Focused: window.IsFocused})
			}
			if len(tabNode.Children) > 0 {
				osNode.Children = append(osNode.Children, tabNode)
			}
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

func quoteKitty(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "$", "$$")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return "\"" + value + "\""
}
