package zka

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenesisTab and GenesisOSWindow describe the desired Kitty tree of a
// workspace born without Kitty, referencing panes by index into
// createWorkspaceRequest.Panes; pane IDs do not exist until the daemon mints
// them. Layout state is deliberately absent: it only exists as a capture of
// real window geometry, and the first live attachment supplies it without
// bumping the topology generation.
type GenesisTab struct {
	Title          string   `json:"title,omitempty"`
	Layout         string   `json:"layout,omitempty"`
	EnabledLayouts []string `json:"enabled_layouts,omitempty"`
	Panes          []int    `json:"panes"`
}

type GenesisOSWindow struct {
	Class string       `json:"class,omitempty"`
	Name  string       `json:"name,omitempty"`
	Tabs  []GenesisTab `json:"tabs"`
}

// GenesisPlan is TemplateGenesis's output; its fields map one-to-one onto
// createWorkspaceRequest.
type GenesisPlan struct {
	Panes     []PaneSpec
	Topology  []GenesisOSWindow
	FocusPane *int
}

// TemplateGenesis interprets a session template the way a real Kitty would
// materialize it, so a workspace born headless matches what `zka kitty` would
// have produced from the same template. GenerateManagedSession deliberately
// never interprets structure — Kitty does that — so this pass exists only for
// births where no Kitty ever runs.
func TemplateGenesis(template SessionTemplate, defaultCWD string) (GenesisPlan, error) {
	plan := GenesisPlan{}
	windows := []GenesisOSWindow{{Tabs: []GenesisTab{{}}}}
	pendingTitle := ""
	lastLaunch := -1
	focus := -1
	currentWindow := func() *GenesisOSWindow { return &windows[len(windows)-1] }
	currentTab := func() *GenesisTab {
		window := currentWindow()
		return &window.Tabs[len(window.Tabs)-1]
	}
	for _, line := range template.Lines {
		if line.IsLaunch {
			spec, err := genesisPaneSpec(line.Launch.Options, defaultCWD, pendingTitle)
			if err != nil {
				return GenesisPlan{}, err
			}
			pendingTitle = ""
			tab := currentTab()
			if option, ok := line.Launch.Options.Get("--tab-title"); ok && option.Value != "" {
				tab.Title = stripStateMarker(option.Value)
			}
			plan.Panes = append(plan.Panes, spec)
			lastLaunch = len(plan.Panes) - 1
			tab.Panes = append(tab.Panes, lastLaunch)
			continue
		}
		value := line.Line.Rest
		switch line.Line.Directive {
		case "new_tab":
			pendingTitle = ""
			tab := currentTab()
			if len(tab.Panes) == 0 {
				// Kitty deletes a preceding tab that never received a window,
				// so its accumulated settings die with it.
				*tab = GenesisTab{Title: stripStateMarker(value)}
				continue
			}
			currentWindow().Tabs = append(currentWindow().Tabs, GenesisTab{Title: stripStateMarker(value)})
		case "new_os_window":
			pendingTitle = ""
			if len(currentTab().Panes) == 0 {
				return GenesisPlan{}, fmt.Errorf("new_os_window follows a tab with no launch; Kitty would fill that tab with an unmanaged shell")
			}
			windows = append(windows, GenesisOSWindow{Tabs: []GenesisTab{{}}})
		case "layout":
			name := strings.TrimSpace(value)
			if !validKittyLayoutName(name) {
				return GenesisPlan{}, fmt.Errorf("unknown layout %q would abort every session load", name)
			}
			currentTab().Layout = name
		case "enabled_layouts":
			var layouts []string
			for _, name := range strings.Split(value, ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if !validKittyLayoutName(name) {
					return GenesisPlan{}, fmt.Errorf("unknown layout %q would abort every session load", name)
				}
				layouts = append(layouts, name)
			}
			currentTab().EnabledLayouts = layouts
		case "os_window_class":
			currentWindow().Class = value
		case "os_window_name":
			currentWindow().Name = value
		case "title":
			pendingTitle = stripStateMarker(value)
		case "focus":
			// Kitty focuses the window of the preceding launch; a focus before
			// any launch is ignored, and the last one wins.
			if lastLaunch >= 0 {
				focus = lastLaunch
			}
		case "set_layout_state":
			return GenesisPlan{}, fmt.Errorf("set_layout_state is not supported for headless creation; layout state is captured from a live Kitty")
		default:
			// cd, os_window_state, os_window_title, os_window_size,
			// resize_window, focus_tab, focus_os_window and
			// focus_matching_window shape only the live view: none of them is
			// captured by topologyFromKitty or replayed by
			// renderDesiredTopologySession, so a headless birth accepts and
			// drops them. cd is already inert for managed panes, which always
			// carry an explicit --cwd.
		}
	}
	if len(currentTab().Panes) == 0 {
		return GenesisPlan{}, fmt.Errorf("template ends with a tab that has no launch; Kitty would fill that tab with an unmanaged shell")
	}
	if focus >= 0 {
		plan.FocusPane = &focus
	}
	plan.Topology = windows
	return plan, nil
}

// genesisPaneSpec builds one pane of a headless birth. Unlike the live-launch
// path, the working directory must be resolved now — there is no Kitty whose
// process environment could resolve it later — and the stored launch options
// drop the fields the pane model owns, matching what applyManifestLaunchOptions
// enforces after every capture.
func genesisPaneSpec(options launchOptions, defaultCWD, pendingTitle string) (PaneSpec, error) {
	spec := paneSpecFromLaunch(options, defaultCWD, pendingTitle)
	if spec.CWD != "" && !filepath.IsAbs(spec.CWD) {
		if defaultCWD == "" {
			return PaneSpec{}, fmt.Errorf("launch --cwd %q is relative and no default directory was given", spec.CWD)
		}
		spec.CWD = filepath.Clean(filepath.Join(defaultCWD, spec.CWD))
	}
	spec.LaunchOptions = options.Drop("--cwd", "--title", "--window-title", "--tab-title").clone()
	return spec, nil
}

// validKittyLayoutName accepts Kitty's seven fixed layouts, with or without
// ":"-separated options. An unknown name is worth rejecting at creation: Kitty
// aborts an entire session load over it, which would make every future attach
// of the workspace fail.
func validKittyLayoutName(name string) bool {
	switch strings.SplitN(name, ":", 2)[0] {
	case "fat", "grid", "horizontal", "splits", "stack", "tall", "vertical":
		return true
	}
	return false
}

// validateGenesisRequest checks a genesis topology before any state exists and
// resolves pane working directories: empty means the daemon user's home, and
// anything else must be absolute on this machine — a silent fallback here
// would surface much later as a remote pane opening in the wrong directory.
func validateGenesisRequest(req *createWorkspaceRequest) error {
	if len(req.Topology) == 0 {
		if req.FocusPane != nil {
			return fmt.Errorf("focus pane requires a genesis topology")
		}
		return nil
	}
	used := make(map[int]bool, len(req.Panes))
	for _, window := range req.Topology {
		if len(window.Tabs) == 0 {
			return fmt.Errorf("genesis os-window has no tabs")
		}
		for _, tab := range window.Tabs {
			if len(tab.Panes) == 0 {
				return fmt.Errorf("genesis tab has no panes")
			}
			if tab.Layout != "" && !validKittyLayoutName(tab.Layout) {
				return fmt.Errorf("unknown layout %q would abort every session load", tab.Layout)
			}
			for _, name := range tab.EnabledLayouts {
				if !validKittyLayoutName(name) {
					return fmt.Errorf("unknown layout %q would abort every session load", name)
				}
			}
			for _, index := range tab.Panes {
				if index < 0 || index >= len(req.Panes) {
					return fmt.Errorf("genesis tab references pane %d, but the request has %d panes", index, len(req.Panes))
				}
				if used[index] {
					return fmt.Errorf("genesis topology places pane %d twice", index)
				}
				used[index] = true
			}
		}
	}
	if len(used) != len(req.Panes) {
		return fmt.Errorf("genesis topology places %d of %d panes", len(used), len(req.Panes))
	}
	if req.FocusPane != nil && (*req.FocusPane < 0 || *req.FocusPane >= len(req.Panes)) {
		return fmt.Errorf("focus pane %d is out of range", *req.FocusPane)
	}
	home := ""
	for i := range req.Panes {
		cwd := strings.TrimSpace(req.Panes[i].CWD)
		if cwd == "" {
			if home == "" {
				resolved, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("resolve default pane directory: %w", err)
				}
				home = resolved
			}
			cwd = home
		}
		if !filepath.IsAbs(cwd) {
			return fmt.Errorf("pane %d working directory %q is not absolute", i, cwd)
		}
		req.Panes[i].CWD = cwd
	}
	return nil
}

// installGenesisTopology builds the desired topology of a workspace born
// without Kitty and installs it through the same primitives a live capture
// uses. Container IDs are left empty for stabilizeTopologyIDs to mint; the
// rendered session then carries them as zka_os_window/zka_tab user variables,
// which is why the first real capture reproduces the same structural digest
// and converges without a generation bump.
func installGenesisTopology(workspace *Workspace, req createWorkspaceRequest, paneIDs []string) error {
	nodes := make([]Node, 0, len(req.Topology))
	for _, window := range req.Topology {
		osNode := Node{Kind: "os-window", Class: window.Class, Name: window.Name}
		for _, tab := range window.Tabs {
			tabNode := Node{
				Kind: "tab", Title: tab.Title, Layout: tab.Layout,
				EnabledLayouts: append([]string(nil), tab.EnabledLayouts...),
			}
			for _, index := range tab.Panes {
				pane := workspace.Panes[paneIDs[index]]
				tabNode.Children = append(tabNode.Children, Node{
					Kind: "pane", ID: pane.ID, PaneID: pane.ID, Title: pane.Title, CWD: pane.CWD,
				})
			}
			osNode.Children = append(osNode.Children, tabNode)
		}
		nodes = append(nodes, osNode)
	}
	if _, err := installDesiredTopology(workspace, nodes, topologyInstallGenesis); err != nil {
		return err
	}
	if req.FocusPane != nil {
		workspace.RestoreFocusPaneID = paneIDs[*req.FocusPane]
	}
	session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
	if err != nil {
		return err
	}
	workspace.Manifest = Manifest{Session: session, Topology: cloneNodes(workspace.Topology.Roots)}
	return nil
}
