package zka

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// attentionSchemaVersion is 2 because counts.total no longer means "nothing is
// wrong": delivery.failed can be non-zero while items is empty. A v1 consumer
// reading only the count would under-report, and the version is what lets it
// detect that. Every change is additive, and the fields that carry detail are
// omitempty, so a healthy snapshot stays as quiet as it was.
const attentionSchemaVersion = 2

// AttentionCounts is the live aggregate of panes that currently need the
// user's attention. It is deliberately not a notification history.
type AttentionCounts struct {
	Total       int `json:"total"`
	Blocked     int `json:"blocked"`
	Error       int `json:"error"`
	Done        int `json:"done"`
	Undelivered int `json:"undelivered"`
}

// NotificationDelivery is the delivery outcome for the exact event an item
// describes. Without it every surface renders a pane that needs you identically
// whether or not you were ever actually told about it.
type NotificationDelivery struct {
	Channel     string    `json:"channel"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
}

// AttentionDelivery is workspace-wide notification health. It is deliberately
// NOT filtered by the predicate that hides resolved items: a channel that cannot
// deliver must stay visible after the pane it failed for is gone, or glancing at
// that pane would erase the only evidence that the channel is broken.
type AttentionDelivery struct {
	Failed    int      `json:"failed,omitempty"`
	Retrying  int      `json:"retrying,omitempty"`
	Channels  []string `json:"channels,omitempty"`
	LastError string   `json:"last_error,omitempty"`
}

// Broken reports whether any channel currently cannot deliver.
func (a AttentionDelivery) Broken() bool {
	return a.Failed > 0 || a.Retrying > 0
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

	Delivery []NotificationDelivery `json:"delivery,omitempty"`
}

// Undelivered reports whether any channel failed to tell the user about this
// exact item.
func (i AttentionItem) Undelivered() bool {
	for _, delivery := range i.Delivery {
		if delivery.Status == "failed" || delivery.Status == "retrying" {
			return true
		}
	}
	return false
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
	Version  int               `json:"version"`
	Paused   bool              `json:"paused"`
	Highest  AgentState        `json:"highest"`
	Counts   AttentionCounts   `json:"counts"`
	Delivery AttentionDelivery `json:"delivery"`
	Items    []AttentionItem   `json:"items"`
}

func buildAttentionSnapshot(state StateData, enabled []AgentState, policy NotificationPolicy) AttentionSnapshot {
	allowed := make(map[AgentState]bool, len(enabled))
	for _, candidate := range enabled {
		allowed[candidate] = true
	}
	snapshot := AttentionSnapshot{
		Version:  attentionSchemaVersion,
		Paused:   state.AttentionPaused,
		Highest:  StateIdle,
		Delivery: aggregateDelivery(state, policy),
		Items:    []AttentionItem{},
	}
	for _, workspace := range state.Workspaces {
		if workspace == nil || workspace.DeletionPending {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane == nil || !allowed[pane.State] {
				continue
			}
			if pane.AttentionSeen == attentionEventIdentity(pane) {
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
				Delivery:       paneDelivery(pane, policy),
			})
			if snapshot.Items[len(snapshot.Items)-1].Undelivered() {
				snapshot.Counts.Undelivered++
			}
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

// paneDelivery projects records for the pane's current event only. Records from
// earlier turns describe news the user has already moved past, so reporting them
// against the current item would be misleading.
func paneDelivery(pane *Pane, policy NotificationPolicy) []NotificationDelivery {
	if len(pane.Notifications) == 0 {
		return nil
	}
	current := eventIdentity(pane)
	var delivery []NotificationDelivery
	for _, record := range pane.SortedNotifications() {
		if !strings.HasSuffix(record.Key, ":"+current) {
			continue
		}
		if !policy.enabled(record.Channel) {
			continue
		}
		status := notificationRecordStatus(record)
		if status == "sent" {
			continue
		}
		delivery = append(delivery, NotificationDelivery{
			Channel:     record.Channel,
			Status:      status,
			Attempts:    record.Attempts,
			LastError:   record.LastError,
			NextRetryAt: record.NextRetryAt,
		})
	}
	return delivery
}

// aggregateDelivery reports channel health across every pane, including panes
// that are no longer actionable. NextRetryAt is absolute and no relative
// duration is included, so two identical states still marshal byte-for-byte and
// the streaming de-duplication in watchAttention keeps working.
func aggregateDelivery(state StateData, policy NotificationPolicy) AttentionDelivery {
	var delivery AttentionDelivery
	channels := map[string]bool{}
	for _, workspace := range state.Workspaces {
		if workspace == nil || workspace.DeletionPending {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane == nil {
				continue
			}
			for _, record := range pane.SortedNotifications() {
				if !policy.enabled(record.Channel) {
					continue
				}
				switch notificationRecordStatus(record) {
				case "failed":
					delivery.Failed++
				case "retrying":
					delivery.Retrying++
				default:
					continue
				}
				channels[record.Channel] = true
				if delivery.LastError == "" {
					delivery.LastError = record.LastError
				}
			}
		}
	}
	for channel := range channels {
		delivery.Channels = append(delivery.Channels, channel)
	}
	sort.Strings(delivery.Channels)
	return delivery
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

func attentionEventIdentity(pane *Pane) string {
	if pane == nil {
		return ""
	}
	transitioned := pane.Evidence.Timestamp
	if transitioned.IsZero() {
		transitioned = pane.UpdatedAt
	}
	return string(pane.State) + ":" + transitioned.Format(time.RFC3339Nano)
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
