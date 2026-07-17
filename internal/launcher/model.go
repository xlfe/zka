package launcher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xlfe/zka/internal/zka"
)

func splitWorkspaces(workspaces []*zka.Workspace, localNodeID string) (visible []*zka.Workspace, remoteHosts []string) {
	hosts := map[string]struct{}{}
	for _, workspace := range workspaces {
		if workspace == nil || workspace.DeletionPending {
			continue
		}
		if workspace.RemoteHost == "" {
			visible = append(visible, workspace)
			continue
		}
		hosts[workspace.RemoteHost] = struct{}{}
		if workspaceKnownToNode(workspace, localNodeID) {
			visible = append(visible, workspace)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		leftAttached := workspaceAttachedToNode(visible[i], localNodeID)
		rightAttached := workspaceAttachedToNode(visible[j], localNodeID)
		if leftAttached != rightAttached {
			return leftAttached
		}
		left, right := strings.ToLower(visible[i].Name), strings.ToLower(visible[j].Name)
		if left != right {
			return left < right
		}
		if visible[i].RemoteHost != visible[j].RemoteHost {
			return visible[i].RemoteHost < visible[j].RemoteHost
		}
		return visible[i].ID < visible[j].ID
	})
	for host := range hosts {
		remoteHosts = append(remoteHosts, host)
	}
	sort.Strings(remoteHosts)
	return visible, remoteHosts
}

func workspaceKnownToNode(workspace *zka.Workspace, nodeID string) bool {
	if workspace == nil || nodeID == "" {
		return false
	}
	for _, attachment := range workspace.Attachments {
		if attachment != nil && attachment.Node.ID == nodeID && strings.HasPrefix(attachment.Endpoint, "unix:") {
			return true
		}
	}
	return false
}

func workspaceAttachedToNode(workspace *zka.Workspace, nodeID string) bool {
	if workspace == nil || nodeID == "" {
		return false
	}
	for _, attachment := range workspace.Attachments {
		if attachment != nil && attachment.Node.ID == nodeID && strings.HasPrefix(attachment.Endpoint, "unix:") &&
			attachment.Status != zka.AttachmentDetached {
			return true
		}
	}
	return false
}

type localWorkspaceItem struct {
	label     string
	workspace *zka.Workspace
	selection int
}

func localWorkspaceItems(workspaces []*zka.Workspace, nodeID string) []localWorkspaceItem {
	items := make([]localWorkspaceItem, 0, len(workspaces)+2)
	for _, section := range []struct {
		label    string
		attached bool
	}{{label: "ATTACHED", attached: true}, {label: "DETACHED", attached: false}} {
		start := len(items)
		items = append(items, localWorkspaceItem{label: section.label})
		for index, workspace := range workspaces {
			if workspaceAttachedToNode(workspace, nodeID) == section.attached {
				items = append(items, localWorkspaceItem{workspace: workspace, selection: index})
			}
		}
		if len(items) == start+1 {
			items = items[:start]
		}
	}
	return items
}

func sortRemoteWorkspaces(workspaces []*zka.Workspace) []*zka.Workspace {
	result := make([]*zka.Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace != nil && !workspace.DeletionPending {
			result = append(result, workspace)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		if left != right {
			return left < right
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func createArgs(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return []string{"kitty"}
	}
	return []string{"kitty", "--name", name}
}

func attachArgs(host string, workspace *zka.Workspace) []string {
	ref := workspace.ID
	if host != "" {
		ref = host + ":" + ref
	}
	return []string{"workspace", "attach", ref}
}

func detachArgs(workspace *zka.Workspace) []string {
	return []string{"workspace", "detach", workspace.ID}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func workspaceSummary(workspace *zka.Workspace) string {
	parts := make([]string, 0, 5)
	if workspace.RemoteHost != "" {
		parts = append(parts, workspace.RemoteHost)
	}
	parts = append(parts,
		workspaceStateSummary(workspace.Attention),
		workspaceAgentSummary(workspace),
		workspaceTopologySummary(workspace),
		shortID(workspace.ID),
	)
	return strings.Join(parts, "  ·  ")
}

func workspaceStateSummary(state zka.AgentState) string {
	switch state {
	case zka.StateWorking:
		return "Working"
	case zka.StateBlocked:
		return "Waiting for you"
	case zka.StateDone:
		return "Finished"
	case zka.StateError:
		return "Failed"
	case zka.StateIdle:
		return "Idle"
	default:
		return "Unknown"
	}
}

func workspaceAgentSummary(workspace *zka.Workspace) string {
	counts := map[string]int{}
	for _, pane := range workspace.Panes {
		if pane == nil {
			continue
		}
		agent := strings.ToLower(strings.TrimSpace(pane.Agent))
		if agent != "" {
			counts[agent]++
		}
	}
	if len(counts) == 0 {
		return "no agent"
	}
	agents := make([]string, 0, len(counts))
	total := 0
	for agent, count := range counts {
		label := agent
		if count > 1 {
			label = fmt.Sprintf("%s ×%d", agent, count)
		}
		agents = append(agents, label)
		total += count
	}
	sort.Strings(agents)
	prefix := "agent: "
	if total > 1 || len(agents) > 1 {
		prefix = "agents: "
	}
	return prefix + strings.Join(agents, ", ")
}

func workspaceTopologySummary(workspace *zka.Workspace) string {
	panes := len(workspace.Panes)
	parts := []string{countLabel(panes, "pane")}
	windows, tabs := topologyCounts(workspace.Manifest.Topology)
	if tabs > 0 || windows > 0 {
		parts = append(parts, countLabel(tabs, "tab"), countLabel(windows, "window"))
	}
	return strings.Join(parts, " / ")
}

func topologyCounts(nodes []zka.Node) (windows, tabs int) {
	for _, node := range nodes {
		switch node.Kind {
		case "os-window":
			windows++
		case "tab":
			tabs++
		}
		childWindows, childTabs := topologyCounts(node.Children)
		windows += childWindows
		tabs += childTabs
	}
	return windows, tabs
}

func countLabel(count int, singular string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", count, singular)
}
