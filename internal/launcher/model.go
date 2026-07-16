package launcher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xlfe/zka/internal/zka"
)

func splitWorkspaces(workspaces []*zka.Workspace) (local []*zka.Workspace, remoteHosts []string) {
	hosts := map[string]struct{}{}
	for _, workspace := range workspaces {
		if workspace == nil || workspace.DeletionPending {
			continue
		}
		if workspace.RemoteHost == "" {
			local = append(local, workspace)
			continue
		}
		hosts[workspace.RemoteHost] = struct{}{}
	}
	sort.SliceStable(local, func(i, j int) bool {
		left, right := strings.ToLower(local[i].Name), strings.ToLower(local[j].Name)
		if left != right {
			return left < right
		}
		return local[i].ID < local[j].ID
	})
	for host := range hosts {
		remoteHosts = append(remoteHosts, host)
	}
	sort.Strings(remoteHosts)
	return local, remoteHosts
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

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func workspaceSummary(workspace *zka.Workspace) string {
	state := workspace.Attention
	if state == "" {
		state = zka.StateUnknown
	}
	paneCount := fmt.Sprintf("%d panes", len(workspace.Panes))
	if len(workspace.Panes) == 1 {
		paneCount = "1 pane"
	}
	return fmt.Sprintf("%s  ·  %s  ·  %s", state, paneCount, shortID(workspace.ID))
}
