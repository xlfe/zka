package zka

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	stateSchemaVersion = 3
	protocolVersion    = 3
	remoteProtocolName = "zka.workspace"
	remoteProtocolMax  = 1 << 20
)

type AgentState string

const (
	StateUnknown AgentState = "unknown"
	StateIdle    AgentState = "idle"
	StateWorking AgentState = "working"
	StateBlocked AgentState = "blocked"
	StateDone    AgentState = "done"
	StateError   AgentState = "error"
)

func (s AgentState) Valid() bool {
	switch s {
	case StateUnknown, StateIdle, StateWorking, StateBlocked, StateDone, StateError:
		return true
	default:
		return false
	}
}

type AttachmentRole string

const (
	AttachmentPrimary AttachmentRole = "primary"
	AttachmentMirror  AttachmentRole = "mirror"
)

type AttachmentStatus string

const (
	AttachmentPreparing AttachmentStatus = "preparing"
	AttachmentReady     AttachmentStatus = "ready"
	AttachmentUnhealthy AttachmentStatus = "unhealthy"
	AttachmentDetached  AttachmentStatus = "detached"
)

type Transport struct {
	Kind string `json:"kind"` // local or ssh
	Host string `json:"host,omitempty"`
}

type Host struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SSHHost  string `json:"ssh_host,omitempty"`
	Platform string `json:"platform,omitempty"`
}

type BackendRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type Evidence struct {
	Source    string    `json:"source"`
	Event     string    `json:"event"`
	Detail    string    `json:"detail,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type ProcessStatus struct {
	Running  bool      `json:"running"`
	PID      int       `json:"pid,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Started  time.Time `json:"started_at,omitempty"`
	Exited   time.Time `json:"exited_at,omitempty"`
}

type NotificationRecord struct {
	Key       string    `json:"key"`
	Channel   string    `json:"channel"`
	SentAt    time.Time `json:"sent_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Pane is the durable identity of one Kitty terminal pane and its hidden zmx
// PTY. Foreground programs are never stored as restore commands.
type Pane struct {
	ID             string                        `json:"id"`
	AllocationKey  string                        `json:"allocation_key,omitempty"`
	Position       int                           `json:"position"`
	Backend        BackendRef                    `json:"backend"`
	CWD            string                        `json:"cwd,omitempty"`
	Title          string                        `json:"title,omitempty"`
	Visible        bool                          `json:"visible"`
	Agent          string                        `json:"agent,omitempty"`
	State          AgentState                    `json:"state"`
	Evidence       Evidence                      `json:"evidence"`
	LastTurnID     string                        `json:"last_turn_id,omitempty"`
	Process        ProcessStatus                 `json:"process"`
	Notifications  map[string]NotificationRecord `json:"notifications,omitempty"`
	BackendCreated bool                          `json:"backend_created"`
	BackendReady   bool                          `json:"backend_ready"`
	BackendStart   bool                          `json:"backend_starting,omitempty"`
	RemovalPending bool                          `json:"removal_pending,omitempty"`
	RemovalError   string                        `json:"removal_error,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

func (p *Pane) Clone() *Pane {
	b, _ := json.Marshal(p)
	var out Pane
	_ = json.Unmarshal(b, &out)
	return &out
}

// Node is a logical Kitty topology node. It intentionally has no Kitty
// runtime IDs; those belong to Attachment.Views.
type Node struct {
	Kind           string          `json:"kind"` // os-window, tab, or pane
	PaneID         string          `json:"pane_id,omitempty"`
	Title          string          `json:"title,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	State          string          `json:"state,omitempty"`
	Class          string          `json:"class,omitempty"`
	Name           string          `json:"name,omitempty"`
	Layout         string          `json:"layout,omitempty"`
	EnabledLayouts []string        `json:"enabled_layouts,omitempty"`
	LayoutState    json.RawMessage `json:"layout_state,omitempty"`
	Active         bool            `json:"active,omitempty"`
	Focused        bool            `json:"focused,omitempty"`
	Children       []Node          `json:"children,omitempty"`
}

type Manifest struct {
	KittyVersion string    `json:"kitty_version,omitempty"`
	CapturedAt   time.Time `json:"captured_at,omitempty"`
	Session      string    `json:"session"`
	Topology     []Node    `json:"topology"`
}

type RuntimeView struct {
	PaneID     string    `json:"pane_id"`
	WindowID   int64     `json:"window_id"`
	TabID      int64     `json:"tab_id,omitempty"`
	OSWindowID int64     `json:"os_window_id,omitempty"`
	Focused    bool      `json:"focused"`
	Ready      bool      `json:"ready"`
	LastSeen   time.Time `json:"last_seen"`
}

type Attachment struct {
	ID               string                 `json:"id"`
	Node             Host                   `json:"node"`
	Transport        Transport              `json:"transport"`
	Role             AttachmentRole         `json:"role"`
	Status           AttachmentStatus       `json:"status"`
	Endpoint         string                 `json:"endpoint,omitempty"`
	PID              int                    `json:"pid,omitempty"`
	AppliedRevision  uint64                 `json:"applied_revision"`
	Views            map[string]RuntimeView `json:"views,omitempty"`
	ClientHeartbeats map[string]time.Time   `json:"client_heartbeats,omitempty"`
	LastError        string                 `json:"last_error,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
	Revoked          bool                   `json:"revoked,omitempty"`
	RevocationClosed bool                   `json:"revocation_closed,omitempty"`
}

func (a *Attachment) Clone() *Attachment {
	b, _ := json.Marshal(a)
	var out Attachment
	_ = json.Unmarshal(b, &out)
	return &out
}

func (a *Attachment) SortedViews() []RuntimeView {
	views := make([]RuntimeView, 0, len(a.Views))
	for _, view := range a.Views {
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].PaneID < views[j].PaneID })
	return views
}

type Workspace struct {
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	Origin              Host                   `json:"origin"`
	RemoteHost          string                 `json:"remote_host,omitempty"`
	Revision            uint64                 `json:"revision"`
	Shell               []string               `json:"shell"`
	Panes               map[string]*Pane       `json:"panes"`
	Manifest            Manifest               `json:"manifest"`
	Attachments         map[string]*Attachment `json:"attachments"`
	PrimaryAttachmentID string                 `json:"primary_attachment_id,omitempty"`
	PendingRevocations  []string               `json:"pending_revocations,omitempty"`
	Attention           AgentState             `json:"attention"`
	DeletionPending     bool                   `json:"deletion_pending,omitempty"`
	DeletionError       string                 `json:"deletion_error,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	UpdatedAt           time.Time              `json:"updated_at"`
}

func (w *Workspace) Clone() *Workspace {
	b, _ := json.Marshal(w)
	var out Workspace
	_ = json.Unmarshal(b, &out)
	return &out
}

func (w *Workspace) RecomputeAttention() AgentState {
	state := StateIdle
	if len(w.Panes) == 0 {
		state = StateUnknown
	}
	for _, pane := range w.Panes {
		if statePriority(pane.State) > statePriority(state) {
			state = pane.State
		}
	}
	w.Attention = state
	return state
}

func (w *Workspace) SortedPanes() []*Pane {
	panes := make([]*Pane, 0, len(w.Panes))
	for _, pane := range w.Panes {
		panes = append(panes, pane.Clone())
	}
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].Position != panes[j].Position {
			return panes[i].Position < panes[j].Position
		}
		if panes[i].CreatedAt.Equal(panes[j].CreatedAt) {
			return panes[i].ID < panes[j].ID
		}
		return panes[i].CreatedAt.Before(panes[j].CreatedAt)
	})
	return panes
}

func (w *Workspace) SortedAttachments() []*Attachment {
	attachments := make([]*Attachment, 0, len(w.Attachments))
	for _, attachment := range w.Attachments {
		attachments = append(attachments, attachment.Clone())
	}
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].ID < attachments[j].ID })
	return attachments
}

type RemoteCache struct {
	Host       string                `json:"host"`
	Workspaces map[string]*Workspace `json:"workspaces"`
	UpdatedAt  time.Time             `json:"updated_at,omitempty"`
	LastError  string                `json:"last_error,omitempty"`
}

type StateData struct {
	SchemaVersion int                     `json:"schema_version"`
	Node          Host                    `json:"node"`
	Workspaces    map[string]*Workspace   `json:"workspaces"`
	Remotes       map[string]*RemoteCache `json:"remotes,omitempty"`
}

func newStateData() StateData {
	return StateData{
		SchemaVersion: stateSchemaVersion,
		Workspaces:    map[string]*Workspace{},
		Remotes:       map[string]*RemoteCache{},
	}
}

type Event struct {
	WorkspaceID string         `json:"workspace_id"`
	PaneID      string         `json:"pane_id"`
	Kind        string         `json:"kind"`
	Source      string         `json:"source"`
	TurnID      string         `json:"turn_id,omitempty"`
	Detail      string         `json:"detail,omitempty"`
	PID         int            `json:"pid,omitempty"`
	ExitCode    *int           `json:"exit_code,omitempty"`
	Fields      map[string]any `json:"fields,omitempty"`
}

type WatcherEvent struct {
	Version   int       `json:"version"`
	Endpoint  string    `json:"endpoint"`
	Workspace string    `json:"workspace,omitempty"`
	Kind      string    `json:"kind"`
	WindowID  int64     `json:"window_id,omitempty"`
	PaneID    string    `json:"pane_id,omitempty"`
	Confirmed bool      `json:"confirmed,omitempty"`
	Aborted   bool      `json:"aborted,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 80 {
		return fmt.Errorf("name must be at most 80 characters")
	}
	if strings.ContainsAny(name, "\x00\r\n:") {
		return fmt.Errorf("name contains a control character")
	}
	return nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func backendName(workspaceID, paneID string) string {
	return "zka-" + shortID(workspaceID) + "-" + shortID(paneID)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func stateMarker(state AgentState) string {
	switch state {
	case StateWorking:
		return "[~]"
	case StateBlocked:
		return "[!]"
	case StateDone:
		return "[✓]"
	case StateError:
		return "[×]"
	case StateUnknown:
		return "[?]"
	default:
		return ""
	}
}
