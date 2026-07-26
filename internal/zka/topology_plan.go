package zka

// The reconciler used to decide what to do from a single whole-tree boolean:
// if the observed digest differed at all, it ran goto_session, which closes and
// re-creates every window in the workspace. A one-character title difference
// cost the user their entire session.
//
// topologyPlan replaces that with an explicit diff. Structural divergence is
// repaired by the narrowest Kitty operation that fixes it; presentation is
// pushed only where it actually differs; and a converged workspace produces an
// empty plan, so a steady-state reconcile issues no Kitty commands at all.

type launchTarget struct {
	OSNodeID   string
	TabNodeID  string
	PaneID     string
	LaunchType string
	Anchor     int64
}

type windowMove struct {
	PaneID      string
	WindowID    int64
	TargetTabID int64 // 0 means "a new tab"
}

type tabMove struct {
	TabNodeID   string
	TabID       int64
	TargetTabID int64 // 0 means "a new OS window"
}

type tabTitleAction struct {
	TabNodeID string
	TabID     int64
	Title     string
}

type tabLayoutAction struct {
	TabNodeID string
	TabID     int64
	Layout    string
	Enabled   []string
}

type topologyPlan struct {
	Close       []int64
	Launch      []launchTarget
	MoveWindows []windowMove
	MoveTabs    []tabMove
	TabTitles   []tabTitleAction
	TabLayouts  []tabLayoutAction
	Focus       string
}

func (p topologyPlan) empty() bool {
	return len(p.Close) == 0 && len(p.Launch) == 0 && len(p.MoveWindows) == 0 &&
		len(p.MoveTabs) == 0 && len(p.TabTitles) == 0 && len(p.TabLayouts) == 0 && p.Focus == ""
}

func (p topologyPlan) structural() bool {
	return len(p.Close) != 0 || len(p.Launch) != 0 || len(p.MoveWindows) != 0 || len(p.MoveTabs) != 0
}

// liveTab is the observed state of one Kitty tab, keyed by its runtime id.
type liveTab struct {
	TabID      int64
	OSWindowID int64
	Panes      []string
	Title      string
	Layout     string
	Enabled    []string
}

func observedTabs(nodes []Node, views map[string]RuntimeView) []liveTab {
	var tabs []liveTab
	for _, osNode := range nodes {
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) == 0 {
				continue
			}
			view := views[tabNode.Children[0].PaneID]
			tab := liveTab{
				TabID: view.TabID, OSWindowID: view.OSWindowID,
				Title: tabNode.Title, Layout: tabNode.Layout, Enabled: tabNode.EnabledLayouts,
			}
			for _, paneNode := range tabNode.Children {
				tab.Panes = append(tab.Panes, paneNode.PaneID)
			}
			tabs = append(tabs, tab)
		}
	}
	return tabs
}

// planTopologyReconcile computes the work needed to bring one attachment to the
// desired topology. It never plans a destructive rebuild: everything structural
// is expressible as launch, close, detach-window or detach-tab.
func planTopologyReconcile(workspace *Workspace, observed []Node, views map[string]RuntimeView, focusedPane string) topologyPlan {
	var plan topologyPlan
	desired := desiredPaneIDs(workspace)

	for paneID, view := range views {
		pane := workspace.Panes[paneID]
		if !desired[paneID] || pane == nil || pane.Retiring() {
			plan.Close = append(plan.Close, view.WindowID)
		}
	}

	anyAnchor := int64(0)
	for _, view := range views {
		if anyAnchor == 0 || view.WindowID < anyAnchor {
			anyAnchor = view.WindowID
		}
	}
	for _, osNode := range workspace.Topology.Roots {
		osAnchor := int64(0)
		for _, tabNode := range osNode.Children {
			for _, paneNode := range tabNode.Children {
				if view, ok := views[paneNode.PaneID]; ok {
					osAnchor = view.WindowID
					break
				}
			}
			if osAnchor != 0 {
				break
			}
		}
		for _, tabNode := range osNode.Children {
			tabAnchor := int64(0)
			for _, paneNode := range tabNode.Children {
				if view, ok := views[paneNode.PaneID]; ok {
					tabAnchor = view.WindowID
					break
				}
			}
			for _, paneNode := range tabNode.Children {
				if _, ok := views[paneNode.PaneID]; ok {
					continue
				}
				target := launchTarget{
					OSNodeID: osNode.ID, TabNodeID: tabNode.ID, PaneID: paneNode.PaneID,
					LaunchType: "window", Anchor: tabAnchor,
				}
				switch {
				case tabAnchor != 0:
				case osAnchor != 0:
					target.LaunchType, target.Anchor = "tab", osAnchor
				default:
					target.LaunchType, target.Anchor = "os-window", anyAnchor
				}
				plan.Launch = append(plan.Launch, target)
			}
		}
	}
	if len(plan.Launch) != 0 || len(plan.Close) != 0 {
		// Grouping and metadata are planned against a settled tree; re-plan
		// once the pane set matches.
		return plan
	}

	live := observedTabs(observed, views)
	liveByTab := map[int64]liveTab{}
	for _, tab := range live {
		liveByTab[tab.TabID] = tab
	}
	claimed := map[int64]bool{}
	for _, osNode := range workspace.Topology.Roots {
		for _, tabNode := range osNode.Children {
			desiredPanes := make([]string, 0, len(tabNode.Children))
			for _, paneNode := range tabNode.Children {
				desiredPanes = append(desiredPanes, paneNode.PaneID)
			}
			if len(desiredPanes) == 0 {
				continue
			}
			tabID := views[desiredPanes[0]].TabID
			actual := liveByTab[tabID]
			if claimed[tabID] || !topologyStringsEqual(actual.Panes, desiredPanes) {
				// Rebuild this tab by pulling its first pane into a fresh tab
				// and moving the rest in behind it, in order.
				plan.MoveWindows = append(plan.MoveWindows, windowMove{
					PaneID: desiredPanes[0], WindowID: views[desiredPanes[0]].WindowID,
				})
				for _, paneID := range desiredPanes[1:] {
					plan.MoveWindows = append(plan.MoveWindows, windowMove{
						PaneID: paneID, WindowID: views[paneID].WindowID, TargetTabID: -1,
					})
				}
				continue
			}
			claimed[tabID] = true
			if want := desiredTabName(workspace, tabNode); canonicalStrippedValue(actual.Title) != canonicalStrippedValue(want) {
				plan.TabTitles = append(plan.TabTitles, tabTitleAction{
					TabNodeID: tabNode.ID, TabID: tabID, Title: want,
				})
			}
			if tabNode.Layout != "" && actual.Layout != tabNode.Layout {
				plan.TabLayouts = append(plan.TabLayouts, tabLayoutAction{
					TabNodeID: tabNode.ID, TabID: tabID,
					Layout: tabNode.Layout, Enabled: tabNode.EnabledLayouts,
				})
			}
		}
	}
	if len(plan.MoveWindows) != 0 {
		return plan
	}

	for _, osNode := range workspace.Topology.Roots {
		var leadPanes []string
		for _, tabNode := range osNode.Children {
			if len(tabNode.Children) != 0 {
				leadPanes = append(leadPanes, tabNode.Children[0].PaneID)
			}
		}
		if len(leadPanes) == 0 {
			continue
		}
		osWindowID := views[leadPanes[0]].OSWindowID
		var actual []string
		for _, tab := range live {
			if tab.OSWindowID == osWindowID && len(tab.Panes) != 0 {
				actual = append(actual, tab.Panes[0])
			}
		}
		if topologyStringsEqual(actual, leadPanes) {
			continue
		}
		plan.MoveTabs = append(plan.MoveTabs, tabMove{TabID: views[leadPanes[0]].TabID})
		for _, paneID := range leadPanes[1:] {
			plan.MoveTabs = append(plan.MoveTabs, tabMove{
				TabID: views[paneID].TabID, TargetTabID: -1,
			})
		}
	}
	if len(plan.MoveTabs) != 0 {
		plan.TabTitles, plan.TabLayouts = nil, nil
		return plan
	}

	// Focus is only asserted when it actually moved, so a converged pass stays
	// completely silent.
	if focusedPane != "" && !views[focusedPane].Focused {
		plan.Focus = focusedPane
	}
	return plan
}

func topologyStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
