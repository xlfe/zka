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
	stateSchemaVersion = 6
	// 9: workspace birth crossed the remote-control protocol
	// (create_workspace). A pre-9 origin would reject the op with a bare
	// "unknown remote operation", so the version check turns skew into an
	// explicit upgrade prompt instead.
	// 8: a replica stopped sending its own working directory when allocating a
	// remote pane, and sends the source pane instead. An origin that predates
	// this would quietly place every remote pane in the home directory, so the
	// version check turns that into an explicit upgrade prompt.
	protocolVersion    = 9
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

type TopologyReconcileStatus string

const (
	TopologyReconcilePending  TopologyReconcileStatus = "pending"
	TopologyReconcileApplying TopologyReconcileStatus = "applying"
	TopologyReconcileReady    TopologyReconcileStatus = "ready"
	TopologyReconcileError    TopologyReconcileStatus = "error"
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

// NotificationRecord is the durable delivery ledger for one (channel, event)
// pair. Attempts and NextRetryAt exist because a failed delivery used to be
// terminal: reserveNotification refused the key forever, so a transient failure
// and a permanently broken channel were indistinguishable, and both were silent.
//
// A record with neither SentAt nor LastError means delivery is in flight. That
// invariant is what makes an abandoned reservation detectable after a crash.
type NotificationRecord struct {
	Key          string    `json:"key"`
	Channel      string    `json:"channel"`
	SentAt       time.Time `json:"sent_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Attempts     int       `json:"attempts,omitempty"`
	FirstTriedAt time.Time `json:"first_tried_at,omitempty"`
	LastTriedAt  time.Time `json:"last_tried_at,omitempty"`
	NextRetryAt  time.Time `json:"next_retry_at,omitempty"`
	Abandoned    bool      `json:"abandoned,omitempty"`
}

// Pane is the durable identity of one Kitty terminal pane and its hidden zmx
// PTY. Foreground programs are never stored as restore commands.
type Pane struct {
	ID                string                        `json:"id"`
	AllocationKey     string                        `json:"allocation_key,omitempty"`
	Position          int                           `json:"position"`
	Backend           BackendRef                    `json:"backend"`
	CWD               string                        `json:"cwd,omitempty"`
	Title             string                        `json:"title,omitempty"`
	LaunchOptions     launchOptions                 `json:"launch_options,omitempty"`
	Agent             string                        `json:"agent,omitempty"`
	State             AgentState                    `json:"state"`
	Evidence          Evidence                      `json:"evidence"`
	LastTurnID        string                        `json:"last_turn_id,omitempty"`
	AttentionSeen     string                        `json:"attention_seen,omitempty"`
	Process           ProcessStatus                 `json:"process"`
	Notifications     map[string]NotificationRecord `json:"notifications,omitempty"`
	BackendCreated    bool                          `json:"backend_created"`
	AgentRelayVersion int                           `json:"agent_relay_version,omitempty"`
	BackendReady      bool                          `json:"backend_ready"`
	BackendStart      bool                          `json:"backend_starting,omitempty"`
	BackendDead       bool                          `json:"backend_dead,omitempty"`
	BackendError      string                        `json:"backend_error,omitempty"`
	RemovalError      string                        `json:"removal_error,omitempty"`
	Phase             PaneLifecycle                 `json:"phase"`
	PhaseAt           time.Time                     `json:"phase_at,omitempty"`
	Admission         PaneAdmission                 `json:"admission,omitempty"`
	CreatedAt         time.Time                     `json:"created_at"`
	UpdatedAt         time.Time                     `json:"updated_at"`
}

// PaneLifecycle replaces the overlapping Visible / TopologyPending /
// RemovalPending flags, which disagreed with each other and with
// Topology.Roots once the desired topology became authoritative.
type PaneLifecycle string

const (
	// PaneProposed: allocated, not yet in the desired topology. It becomes
	// admitted only by committing a Kitty capture that already contains its
	// window -- never by fabricating a topology node for it.
	PaneProposed PaneLifecycle = "proposed"
	// PaneAdmitted: a member of the desired pane set.
	PaneAdmitted PaneLifecycle = "admitted"
	// PaneRetiring: scheduled for removal, already out of the desired topology.
	PaneRetiring PaneLifecycle = "retiring"
)

func (p *Pane) Proposed() bool { return p.Phase == PaneProposed }
func (p *Pane) Admitted() bool { return p.Phase == PaneAdmitted }
func (p *Pane) Retiring() bool { return p.Phase == PaneRetiring }

// PaneAdmission records which Kitty window a proposed pane belongs to, so
// admission can be decided from evidence instead of from a timer.
type PaneAdmission struct {
	Endpoint     string    `json:"endpoint,omitempty"`
	AttachmentID string    `json:"attachment_id,omitempty"`
	WindowID     int64     `json:"window_id,omitempty"`
	RequestedAt  time.Time `json:"requested_at,omitempty"`
	// MissingSince is the first *successful* Kitty listing that lacked the
	// window. Only successful listings set it, so an RPC outage can never
	// cause a pane to be retired.
	MissingSince time.Time `json:"missing_since,omitempty"`
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
	ID             string          `json:"id,omitempty"`
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

// DesiredTopology is the origin-owned logical Kitty tree. Runtime Kitty IDs,
// focus, and viewport state are deliberately excluded from its identity.
type DesiredTopology struct {
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
	Roots      []Node `json:"roots"`
}

type Manifest struct {
	KittyVersion string    `json:"kitty_version,omitempty"`
	CapturedAt   time.Time `json:"captured_at,omitempty"`
	Session      string    `json:"session"`
	Topology     []Node    `json:"topology"`
}

type RuntimeView struct {
	PaneID         string    `json:"pane_id"`
	WindowID       int64     `json:"window_id"`
	TabID          int64     `json:"tab_id,omitempty"`
	OSWindowID     int64     `json:"os_window_id,omitempty"`
	TabNodeID      string    `json:"tab_node_id,omitempty"`
	OSWindowNodeID string    `json:"os_window_node_id,omitempty"`
	Focused        bool      `json:"focused"`
	Ready          bool      `json:"ready"`
	LastSeen       time.Time `json:"last_seen"`
}

type Attachment struct {
	ID                        string                  `json:"id"`
	Node                      Host                    `json:"node"`
	Transport                 Transport               `json:"transport"`
	Role                      AttachmentRole          `json:"role"`
	Status                    AttachmentStatus        `json:"status"`
	Endpoint                  string                  `json:"endpoint,omitempty"`
	PID                       int                     `json:"pid,omitempty"`
	AppliedRevision           uint64                  `json:"applied_revision"`
	AppliedTopologyGeneration uint64                  `json:"applied_topology_generation,omitempty"`
	AppliedTopologyDigest     string                  `json:"applied_topology_digest,omitempty"`
	ObservedTopology          []Node                  `json:"observed_topology,omitempty"`
	ReconcileTargetGeneration uint64                  `json:"reconcile_target_generation,omitempty"`
	ReconcileStatus           TopologyReconcileStatus `json:"reconcile_status,omitempty"`
	Views                     map[string]RuntimeView  `json:"views,omitempty"`
	ClientHeartbeats          map[string]time.Time    `json:"client_heartbeats,omitempty"`
	LastError                 string                  `json:"last_error,omitempty"`
	CreatedAt                 time.Time               `json:"created_at"`
	UpdatedAt                 time.Time               `json:"updated_at"`
	Revoked                   bool                    `json:"revoked,omitempty"`
	RevocationClosed          bool                    `json:"revocation_closed,omitempty"`
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
	ID   string `json:"id"`
	Name string `json:"name"`
	// CreationKey deduplicates replays of the same birth request after a
	// dropped SSH response; see createWorkspaceRequest.CreationKey.
	CreationKey         string                 `json:"creation_key,omitempty"`
	Origin              Host                   `json:"origin"`
	RemoteHost          string                 `json:"remote_host,omitempty"`
	Revision            uint64                 `json:"revision"`
	Shell               []string               `json:"shell"`
	Panes               map[string]*Pane       `json:"panes"`
	Topology            DesiredTopology        `json:"topology"`
	Manifest            Manifest               `json:"manifest"`
	Attachments         map[string]*Attachment `json:"attachments"`
	PrimaryAttachmentID string                 `json:"primary_attachment_id,omitempty"`
	AgentAttachmentID   string                 `json:"agent_attachment_id,omitempty"`
	PendingRevocations  []string               `json:"pending_revocations,omitempty"`
	Attention           AgentState             `json:"attention"`
	RestoreFocusPaneID  string                 `json:"restore_focus_pane_id,omitempty"`
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
	SchemaVersion   int                     `json:"schema_version"`
	Node            Host                    `json:"node"`
	Workspaces      map[string]*Workspace   `json:"workspaces"`
	Remotes         map[string]*RemoteCache `json:"remotes,omitempty"`
	AttentionPaused bool                    `json:"attention_paused,omitempty"`
}

func newStateData() StateData {
	return StateData{
		SchemaVersion: stateSchemaVersion,
		Workspaces:    map[string]*Workspace{},
		Remotes:       map[string]*RemoteCache{},
	}
}

type Event struct {
	WorkspaceID       string         `json:"workspace_id"`
	PaneID            string         `json:"pane_id"`
	Kind              string         `json:"kind"`
	Source            string         `json:"source"`
	TurnID            string         `json:"turn_id,omitempty"`
	Detail            string         `json:"detail,omitempty"`
	PID               int            `json:"pid,omitempty"`
	AgentRelayVersion int            `json:"agent_relay_version,omitempty"`
	ExitCode          *int           `json:"exit_code,omitempty"`
	Fields            map[string]any `json:"fields,omitempty"`
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
