package zka

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
				w := &windows[oi].Tabs[ti].Windows[wi]
				if w.UserVars == nil {
					w.UserVars = map[string]string{}
				}
			}
		}
	}
	return windows, nil
}

func (k KittyClient) NativeSession(ctx context.Context, endpoint string) (string, error) {
	return k.rc(ctx, endpoint, "ls", "--match", "var:zka_session", "--output-format=session")
}

func (k KittyClient) Version(ctx context.Context) string {
	out, _, err := k.Runner.Run(ctx, k.command(), "--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

type LaunchOptions struct {
	Endpoint  string
	Type      string
	CWD       string
	Title     string
	SessionID string
	Backend   string
	State     AgentState
	NewView   bool
}

func (k KittyClient) Launch(ctx context.Context, opts LaunchOptions) (int64, error) {
	typ := opts.Type
	if typ == "" {
		typ = "window"
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find zka executable: %w", err)
	}
	args := []string{"launch", "--type", typ, "--title", opts.Title,
		"--var", "zka_session=" + opts.SessionID,
		"--var", "zka_backend=" + opts.Backend,
		"--var", "zka_state=" + string(opts.State),
		"--env", "ZKA_SESSION_ID=" + opts.SessionID,
	}
	if opts.CWD != "" {
		args = append(args, "--cwd", opts.CWD)
	}
	if typ == "tab" {
		args = append(args, "--tab-title", opts.Title)
	}
	args = append(args, exe, "view", opts.SessionID)
	out, err := k.rc(ctx, opts.Endpoint, args...)
	if err != nil {
		return 0, fmt.Errorf("launch kitty view: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse kitty window id %q: %w", strings.TrimSpace(out), err)
	}
	return id, nil
}

func (k KittyClient) Focus(ctx context.Context, endpoint, sessionID string) error {
	_, err := k.rc(ctx, endpoint, "focus-window", "--match", "var:zka_session="+sessionID)
	return err
}

func (k KittyClient) SetState(ctx context.Context, endpoint string, windowID int64, session *Session) error {
	match := "id:" + strconv.FormatInt(windowID, 10)
	if _, err := k.rc(ctx, endpoint, "set-user-vars", "--match", match, "zka_state="+string(session.State)); err != nil {
		return err
	}
	title := strings.TrimSpace(stateMarker(session.State) + " " + session.Name)
	if _, err := k.rc(ctx, endpoint, "set-window-title", "--match", match, title); err != nil {
		return err
	}
	return nil
}

func (k KittyClient) SetTabTitle(ctx context.Context, endpoint string, tabID int64, title string) error {
	_, err := k.rc(ctx, endpoint, "set-tab-title", "--match", "id:"+strconv.FormatInt(tabID, 10), title)
	return err
}

func (k KittyClient) LoadSession(ctx context.Context, endpoint, path string) error {
	_, err := k.rc(ctx, endpoint, "action", "goto_session "+quoteAction(path))
	return err
}

func (k KittyClient) Notify(ctx context.Context, view View, session *Session) (string, error) {
	urgency := "normal"
	icon := "info"
	switch session.State {
	case StateBlocked:
		urgency, icon = "critical", "question"
	case StateError:
		urgency, icon = "critical", "error"
	case StateDone:
		icon = "info"
	}
	title := notificationTitle(session)
	body := notificationBody(session)
	identifier := "zka-" + session.ID
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	return k.rc(ctx, view.Endpoint, "run", k.command(), "notify",
		"--app-name", "zka", "--identifier", identifier,
		"--urgency", urgency, "--icon", icon,
		"--button", "Focus", "--wait-for-completion", title, body)
}

func (k KittyClient) CloseNotification(ctx context.Context, view View, sessionID string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = k.rc(ctx, view.Endpoint, "run", k.command(), "notify", "--identifier", "zka-"+sessionID)
}

func findManagedViews(tree []kittyOSWindow) map[string][]View {
	result := map[string][]View{}
	now := time.Now().UTC()
	for _, osw := range tree {
		for _, tab := range osw.Tabs {
			for _, win := range tab.Windows {
				id := win.UserVars["zka_session"]
				if id == "" {
					continue
				}
				result[id] = append(result[id], View{
					WindowID: win.ID,
					Attached: true,
					Focused:  win.IsFocused || (tab.IsFocused && win.IsActive) || (osw.IsFocused && tab.IsActive && win.IsActive),
					LastSeen: now,
				})
			}
		}
	}
	return result
}

func quoteKitty(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "$", "$$")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return "\"" + value + "\""
}

func kittyDirective(value string) string {
	value = strings.ReplaceAll(value, "$", "$$")
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

func quoteAction(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return "\"" + value + "\""
}
