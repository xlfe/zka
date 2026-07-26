package zka

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// fakeKitty models Kitty's session loading closely enough to prove the desired
// topology is reproducible. It implements kitty/session.py parse_session:
// splitlines, strip, skip blanks and comments, split once on whitespace,
// expandvars everywhere except launch and set_layout_state, empty-tab deletion
// in add_tab, and the unmanaged shell that finalize_session puts in a trailing
// empty tab.
//
// The outage this package now guards against was zka writing a value Kitty
// could not hand back. Only a model of Kitty's own parsing can catch that
// class; comparing zka against itself cannot. kittyoracle_test.go pins this
// model against the real parser when a Kitty install is available.
type fakeKitty struct {
	osWindows []fakeOSWindow
	nextID    int64
}

type fakeOSWindow struct {
	class, name, state string
	tabs               []*fakeTab
	focusTab           int
}

type fakeTab struct {
	name        string
	layout      string
	enabled     []string
	layoutState string
	windows     []fakeWindow
	nextTitle   string
}

type fakeWindow struct {
	id        int64
	title     string
	cwd       string
	vars      map[string]string
	cmdline   []string
	unmanaged bool
}

// LoadSession replays a rendered session file the way Kitty would.
func (k *fakeKitty) LoadSession(content string) error {
	k.osWindows = []fakeOSWindow{{}}
	current := func() *fakeOSWindow { return &k.osWindows[len(k.osWindows)-1] }
	addTab := func(name string) {
		window := current()
		// session.py add_tab: an empty preceding tab is deleted outright.
		if len(window.tabs) != 0 && len(window.tabs[len(window.tabs)-1].windows) == 0 {
			window.tabs = window.tabs[:len(window.tabs)-1]
		}
		window.tabs = append(window.tabs, &fakeTab{name: strings.TrimFunc(name, pythonIsSpace)})
	}
	lastTab := func() (*fakeTab, error) {
		window := current()
		if len(window.tabs) == 0 {
			addTab("")
		}
		return window.tabs[len(window.tabs)-1], nil
	}
	addTab("")

	for _, line := range parseSessionLines(content) {
		// Kitty expands every directive except these two.
		rest := line.Rest
		if line.Directive != "launch" && line.Directive != "set_layout_state" {
			rest = unescapeExpandVars(rest)
		}
		switch line.Directive {
		case "new_os_window":
			k.osWindows = append(k.osWindows, fakeOSWindow{})
			addTab(rest)
		case "new_tab":
			addTab(rest)
		case "os_window_class":
			current().class = rest
		case "os_window_name":
			current().name = rest
		case "os_window_state":
			current().state = rest
		case "layout":
			tab, _ := lastTab()
			// session.py set_layout raises on an unknown name, which aborts
			// the entire load rather than just this tab.
			if !knownKittyLayout(rest) {
				return fmt.Errorf("%q is not a valid layout", rest)
			}
			tab.layout = rest
		case "enabled_layouts":
			tab, _ := lastTab()
			var names []string
			for _, name := range strings.Split(rest, ",") {
				name = strings.TrimFunc(name, pythonIsSpace)
				if !knownKittyLayout(name) {
					return fmt.Errorf("the window layout %q is unknown", name)
				}
				names = append(names, name)
			}
			tab.enabled = names
		case "set_layout_state":
			tab, _ := lastTab()
			if !json.Valid([]byte(rest)) {
				return fmt.Errorf("set_layout_state is not valid json: %q", rest)
			}
			tab.layoutState = rest
		case "title":
			tab, _ := lastTab()
			tab.nextTitle = rest
		case "focus_tab":
			index, err := strconv.Atoi(rest)
			if err != nil {
				return fmt.Errorf("focus_tab %q is not an index", rest)
			}
			current().focusTab = index
		case "focus", "focus_os_window", "cd", "os_window_title", "focus_matching_window",
			"resize_window", "os_window_size":
		case "launch":
			if err := k.addWindow(line.Rest); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown command in session file: %s", line.Directive)
		}
	}
	// session.py finalize_session: a tab with no windows gets an unmanaged
	// shell, which then shows up in `ls` as an untagged window.
	for i := range k.osWindows {
		for _, tab := range k.osWindows[i].tabs {
			if len(tab.windows) == 0 {
				k.nextID++
				tab.windows = append(tab.windows, fakeWindow{
					id: k.nextID, title: "shell", vars: map[string]string{},
					cmdline: []string{"/bin/sh"}, unmanaged: true,
				})
			}
		}
	}
	return nil
}

func (k *fakeKitty) addWindow(rest string) error {
	tokens, err := shlexSplit(rest)
	if err != nil {
		return err
	}
	if len(tokens) != 0 && tokens[0] == "launch" {
		tokens = tokens[1:]
	}
	if len(tokens) != 0 && strings.HasPrefix(tokens[0], "kitty-unserialize-data=") {
		// Removed before the expansion pass, so it is never expanded.
		tokens = tokens[1:]
	}
	limit := len(tokens)
	for index, token := range tokens {
		if token == "--" {
			limit = index
			break
		}
		if !strings.HasPrefix(token, "-") {
			limit = index
			break
		}
	}
	expanded := make([]string, 0, len(tokens))
	for index, token := range tokens {
		if index < limit {
			// Kitty expands launch *options* but never program arguments.
			token = unescapeExpandVars(token)
		}
		expanded = append(expanded, token)
	}
	options, args, err := splitLaunchTokens(expanded)
	if err != nil {
		return err
	}
	tab, _ := func() (*fakeTab, error) {
		window := &k.osWindows[len(k.osWindows)-1]
		if len(window.tabs) == 0 {
			window.tabs = append(window.tabs, &fakeTab{})
		}
		return window.tabs[len(window.tabs)-1], nil
	}()
	k.nextID++
	window := fakeWindow{id: k.nextID, vars: map[string]string{}, cmdline: args}
	for _, option := range options {
		switch option.Name {
		case "--var":
			if at := strings.IndexByte(option.Value, '='); at >= 0 {
				window.vars[option.Value[:at]] = option.Value[at+1:]
			}
		case "--cwd":
			window.cwd = option.Value
		case "--title", "--window-title":
			window.title = option.Value
		}
	}
	if window.title == "" {
		window.title = tab.nextTitle
	}
	tab.nextTitle = ""
	tab.windows = append(tab.windows, window)
	return nil
}

// LS renders what `kitten @ ls` would report for the loaded session.
func (k *fakeKitty) LS() []kittyOSWindow {
	var tree []kittyOSWindow
	for index := range k.osWindows {
		source := k.osWindows[index]
		osWindow := kittyOSWindow{
			ID: int64(index + 1), WMClass: source.class, WMName: source.name,
			State: source.state, IsFocused: index == 0,
		}
		for tabIndex, tab := range source.tabs {
			layout := tab.layout
			if layout == "" {
				layout = "splits"
			}
			enabled := tab.enabled
			if len(enabled) == 0 {
				// Kitty always reports the process-wide default, never an
				// empty list. A desired tab that omits it is unreproducible.
				enabled = []string{"fat", "grid", "horizontal", "splits", "stack", "tall", "vertical"}
			}
			named := tab.name != ""
			title := tab.name
			var groups []string
			out := kittyTab{
				ID: int64(1000 + index*100 + tabIndex), Layout: layout, Enabled: enabled,
				IsActive: tabIndex == source.focusTab, TitleOverridden: &named,
			}
			for windowIndex, window := range tab.windows {
				out.Windows = append(out.Windows, kittyWindow{
					ID: window.id, Title: window.title, CWD: window.cwd,
					UserVars: window.vars, Cmdline: window.cmdline,
					IsActive: windowIndex == 0,
				})
				groups = append(groups, fmt.Sprintf(`{"id":%d,"window_ids":[%d]}`, window.id, window.id))
			}
			if title == "" && len(tab.windows) != 0 {
				// Kitty reports `tab.name or tab.title`, and tab.title is the
				// active window's live title.
				title = tab.windows[0].title
			}
			out.Title = title
			// Kitty always reports a layout state for a live tab.
			out.LayoutState = json.RawMessage(fmt.Sprintf(
				`{"all_windows":{"active_group_idx":0,"active_group_history":[],"window_groups":[%s]},"class":"Splits","opts":{}}`,
				strings.Join(groups, ",")))
			osWindow.Tabs = append(osWindow.Tabs, out)
		}
		tree = append(tree, osWindow)
	}
	return tree
}

func knownKittyLayout(name string) bool {
	switch strings.SplitN(name, ":", 2)[0] {
	case "fat", "grid", "horizontal", "splits", "stack", "tall", "vertical":
		return true
	}
	return false
}
