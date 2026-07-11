package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func CaptureSnapshot(ctx context.Context, kitty KittyClient, endpoint, name string) (Snapshot, error) {
	if err := validateSnapshotName(name); err != nil {
		return Snapshot{}, err
	}
	tree, err := kitty.List(ctx, endpoint)
	if err != nil {
		return Snapshot{}, err
	}
	native, _ := kitty.NativeSession(ctx, endpoint)
	native = canonicalizeNativeSession(native)
	snapshot := Snapshot{
		SchemaVersion: snapshotSchemaVersion,
		Name:          name,
		CreatedAt:     time.Now().UTC(),
		KittyVersion:  kitty.Version(ctx),
		Source:        endpoint,
		NativeSession: native,
	}
	for _, osw := range tree {
		so := SnapshotOSWindow{State: osw.State, Class: osw.WMClass, Name: osw.WMName, Focused: osw.IsFocused}
		for _, tab := range osw.Tabs {
			st := SnapshotTab{Title: tab.Title, Layout: tab.Layout, Enabled: tab.Enabled, LayoutState: tab.LayoutState, Active: tab.IsActive || tab.IsFocused}
			for _, win := range tab.Windows {
				sessionID := win.UserVars["zka_session"]
				if sessionID == "" {
					continue
				}
				st.Views = append(st.Views, SnapshotView{
					SessionID: sessionID,
					Title:     win.Title,
					CWD:       win.CWD,
					Active:    win.IsActive || win.IsFocused,
				})
			}
			if len(st.Views) > 0 {
				so.Tabs = append(so.Tabs, st)
			}
		}
		if len(so.Tabs) > 0 {
			snapshot.OSWindows = append(snapshot.OSWindows, so)
		}
	}
	if len(snapshot.OSWindows) == 0 {
		return Snapshot{}, fmt.Errorf("no kitty windows tagged with zka_session")
	}
	return snapshot, nil
}

func canonicalizeNativeSession(content string) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return content
	}
	content = strings.ReplaceAll(content, exe, "zka")
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		content = strings.ReplaceAll(content, resolved, "zka")
	}
	return content
}

func nativeSessionIsManaged(content string, sessionIDs []string) bool {
	wanted := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		wanted[id] = true
	}
	launches := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "launch ") {
			continue
		}
		launches++
		matched := false
		for id := range wanted {
			if strings.Contains(line, " zka view "+id) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return launches == len(sessionIDs) && launches > 0
}

func SnapshotSessionIDs(snapshot Snapshot) []string {
	seen := map[string]bool{}
	var result []string
	for _, osw := range snapshot.OSWindows {
		for _, tab := range osw.Tabs {
			for _, view := range tab.Views {
				if !seen[view.SessionID] {
					seen[view.SessionID] = true
					result = append(result, view.SessionID)
				}
			}
		}
	}
	return result
}

func GenerateKittySession(snapshot Snapshot, sessions map[string]*Session, skip map[string]bool) (string, error) {
	var out bytes.Buffer
	osWritten := 0
	for _, osw := range snapshot.OSWindows {
		var tabs []SnapshotTab
		for _, tab := range osw.Tabs {
			filtered := SnapshotTab{Title: tab.Title, Layout: tab.Layout, Enabled: tab.Enabled, LayoutState: tab.LayoutState, Active: tab.Active}
			for _, view := range tab.Views {
				if !skip[view.SessionID] {
					filtered.Views = append(filtered.Views, view)
				}
			}
			if len(filtered.Views) > 0 {
				tabs = append(tabs, filtered)
			}
		}
		if len(tabs) == 0 {
			continue
		}
		if osWritten > 0 {
			out.WriteString("new_os_window\n")
		}
		osWritten++
		if osw.State != "" {
			fmt.Fprintf(&out, "os_window_state %s\n", kittyDirective(osw.State))
		}
		if osw.Class != "" {
			fmt.Fprintf(&out, "os_window_class %s\n", kittyDirective(osw.Class))
		}
		if osw.Name != "" {
			fmt.Fprintf(&out, "os_window_name %s\n", kittyDirective(osw.Name))
		}
		for tabIndex, tab := range tabs {
			fmt.Fprintf(&out, "new_tab %s\n", kittyDirective(tab.Title))
			if len(tab.Enabled) > 0 {
				enabled := strings.Join(tab.Enabled, ",")
				enabled = kittyDirective(enabled)
				fmt.Fprintf(&out, "enabled_layouts %s\n", enabled)
			}
			if tab.Layout != "" {
				fmt.Fprintf(&out, "layout %s\n", kittyDirective(tab.Layout))
			}
			for _, view := range tab.Views {
				session, ok := sessions[view.SessionID]
				if !ok {
					return "", fmt.Errorf("snapshot references unknown session %s", view.SessionID)
				}
				fmt.Fprintf(&out, "launch --hold --title %s --cwd %s --var zka_session=%s --var zka_backend=%s --var zka_state=%s --env ZKA_SESSION_ID=%s zka view %s\n",
					quoteKitty(view.Title), quoteKitty(view.CWD), session.ID, session.Backend.Kind, session.State, session.ID, session.ID)
				if view.Active {
					out.WriteString("focus\n")
				}
			}
			if tab.Active {
				fmt.Fprintf(&out, "focus_tab %d\n", tabIndex)
			}
		}
		if osw.Focused {
			out.WriteString("focus_os_window\n")
		}
	}
	if osWritten == 0 {
		return "", fmt.Errorf("all snapshot views already exist in the target kitty")
	}
	return out.String(), nil
}

func writeRestoreSession(paths Paths, snapshot Snapshot, content string) (string, error) {
	dir := filepath.Join(paths.SnapshotDir, "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create generated session directory: %w", err)
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, snapshot.Name)
	path := filepath.Join(dir, name+".kitty-session")
	if err := atomicWrite(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func snapshotJSON(snapshot Snapshot) ([]byte, error) {
	return json.MarshalIndent(snapshot, "", "  ")
}
