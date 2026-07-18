package zka

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const attentionSchemaVersion = 1

// AttentionCounts is the live aggregate of panes that currently need the
// user's attention. It is deliberately not a notification history.
type AttentionCounts struct {
	Total   int `json:"total"`
	Blocked int `json:"blocked"`
	Error   int `json:"error"`
	Done    int `json:"done"`
}

// AttentionItem identifies one actionable pane. ID remains stable while the
// pane changes state so consumers can update a row rather than append events.
type AttentionItem struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	WorkspaceName  string     `json:"workspace_name"`
	PaneID         string     `json:"pane_id"`
	PaneTitle      string     `json:"pane_title"`
	Origin         string     `json:"origin"`
	RemoteHost     string     `json:"remote_host,omitempty"`
	Agent          string     `json:"agent,omitempty"`
	State          AgentState `json:"state"`
	Detail         string     `json:"detail,omitempty"`
	Evidence       string     `json:"evidence,omitempty"`
	TransitionedAt time.Time  `json:"transitioned_at"`
	Attached       bool       `json:"attached"`
	Focused        bool       `json:"focused"`
}

// WorkspaceRef returns the CLI reference that can restore or focus this item.
func (i AttentionItem) WorkspaceRef() string {
	if i.RemoteHost != "" {
		return i.RemoteHost + ":" + i.WorkspaceID
	}
	return i.WorkspaceID
}

// AttentionSnapshot is a versioned, deterministic projection of live state.
// It intentionally has no generated-at field so identical snapshots compare
// byte-for-byte for streaming de-duplication.
type AttentionSnapshot struct {
	Version int             `json:"version"`
	Paused  bool            `json:"paused"`
	Highest AgentState      `json:"highest"`
	Counts  AttentionCounts `json:"counts"`
	Items   []AttentionItem `json:"items"`
}

func buildAttentionSnapshot(state StateData, enabled []AgentState) AttentionSnapshot {
	allowed := make(map[AgentState]bool, len(enabled))
	for _, candidate := range enabled {
		allowed[candidate] = true
	}
	snapshot := AttentionSnapshot{
		Version: attentionSchemaVersion,
		Paused:  state.AttentionPaused,
		Highest: StateIdle,
		Items:   []AttentionItem{},
	}
	for _, workspace := range state.Workspaces {
		if workspace == nil || workspace.DeletionPending {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane == nil || !allowed[pane.State] {
				continue
			}
			attached, _ := attentionPaneViewOnNode(state.Node.ID, workspace, pane.ID)
			_, focused := attentionPaneView(workspace, pane.ID)
			if pane.State == StateDone && focused {
				continue
			}
			transitioned := pane.Evidence.Timestamp
			if transitioned.IsZero() {
				transitioned = pane.UpdatedAt
			}
			detail := strings.TrimSpace(pane.Evidence.Detail)
			if detail == "" {
				detail = strings.TrimSpace(pane.Evidence.Event)
			}
			origin := workspace.Origin.Name
			if origin == "" {
				origin = workspace.RemoteHost
			}
			snapshot.Items = append(snapshot.Items, AttentionItem{
				ID:             attentionItemID(workspace, pane),
				WorkspaceID:    workspace.ID,
				WorkspaceName:  workspace.Name,
				PaneID:         pane.ID,
				PaneTitle:      pane.Title,
				Origin:         origin,
				RemoteHost:     workspace.RemoteHost,
				Agent:          pane.Agent,
				State:          pane.State,
				Detail:         detail,
				Evidence:       strings.Trim(strings.TrimSpace(pane.Evidence.Source+"/"+pane.Evidence.Event), "/"),
				TransitionedAt: transitioned,
				Attached:       attached,
				Focused:        focused,
			})
			switch pane.State {
			case StateBlocked:
				snapshot.Counts.Blocked++
			case StateError:
				snapshot.Counts.Error++
			case StateDone:
				snapshot.Counts.Done++
			}
		}
	}
	sort.Slice(snapshot.Items, func(i, j int) bool {
		left, right := snapshot.Items[i], snapshot.Items[j]
		if attentionPriority(left.State) != attentionPriority(right.State) {
			return attentionPriority(left.State) > attentionPriority(right.State)
		}
		if !left.TransitionedAt.Equal(right.TransitionedAt) {
			if left.TransitionedAt.IsZero() {
				return false
			}
			if right.TransitionedAt.IsZero() {
				return true
			}
			return left.TransitionedAt.Before(right.TransitionedAt)
		}
		if strings.ToLower(left.WorkspaceName) != strings.ToLower(right.WorkspaceName) {
			return strings.ToLower(left.WorkspaceName) < strings.ToLower(right.WorkspaceName)
		}
		if left.WorkspaceID != right.WorkspaceID {
			return left.WorkspaceID < right.WorkspaceID
		}
		return left.PaneID < right.PaneID
	})
	snapshot.Counts.Total = len(snapshot.Items)
	if len(snapshot.Items) > 0 {
		snapshot.Highest = snapshot.Items[0].State
	}
	return snapshot
}

func attentionPriority(state AgentState) int {
	switch state {
	case StateBlocked:
		return 3
	case StateError:
		return 2
	case StateDone:
		return 1
	default:
		return 0
	}
}

func attentionPaneView(workspace *Workspace, paneID string) (attached, focused bool) {
	return attentionPaneViewOnNode("", workspace, paneID)
}

func attentionPaneViewOnNode(nodeID string, workspace *Workspace, paneID string) (attached, focused bool) {
	for _, attachment := range workspace.Attachments {
		if attachment == nil || (nodeID != "" && attachment.Node.ID != nodeID) || attachment.Status == AttachmentDetached ||
			attachment.Revoked || !strings.HasPrefix(attachment.Endpoint, "unix:") {
			continue
		}
		if view, ok := attachment.Views[paneID]; ok && view.Ready {
			attached = true
			focused = focused || view.Focused
		}
	}
	return attached, focused
}

func attentionItemID(workspace *Workspace, pane *Pane) string {
	origin := workspace.Origin.ID
	if origin == "" {
		origin = workspace.RemoteHost
	}
	if origin == "" {
		origin = "local"
	}
	return fmt.Sprintf("%s:%s:%s", origin, workspace.ID, pane.ID)
}

func nextAttentionItem(snapshot AttentionSnapshot) (AttentionItem, bool) {
	for _, item := range snapshot.Items {
		if !item.Focused {
			return item, true
		}
	}
	if len(snapshot.Items) > 0 {
		return snapshot.Items[0], true
	}
	return AttentionItem{}, false
}
