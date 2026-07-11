package zka

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	stateSchemaVersion    = 1
	snapshotSchemaVersion = 1
	protocolVersion       = 1
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

type View struct {
	Endpoint string    `json:"endpoint"`
	WindowID int64     `json:"window_id"`
	Attached bool      `json:"attached"`
	Focused  bool      `json:"focused"`
	LastSeen time.Time `json:"last_seen"`
}

func (v View) Key() string { return fmt.Sprintf("%s#%d", v.Endpoint, v.WindowID) }

type NotificationRecord struct {
	Key       string    `json:"key"`
	Channel   string    `json:"channel"`
	SentAt    time.Time `json:"sent_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

type Session struct {
	ID             string                        `json:"id"`
	Name           string                        `json:"name"`
	Backend        BackendRef                    `json:"backend"`
	Agent          string                        `json:"agent"`
	Command        []string                      `json:"command"`
	CWD            string                        `json:"cwd"`
	State          AgentState                    `json:"state"`
	Evidence       Evidence                      `json:"evidence"`
	LastTurnID     string                        `json:"last_turn_id,omitempty"`
	Process        ProcessStatus                 `json:"process"`
	Views          map[string]View               `json:"views,omitempty"`
	Notifications  map[string]NotificationRecord `json:"notifications,omitempty"`
	BackendCreated bool                          `json:"backend_created"`
	BackendReady   bool                          `json:"backend_ready"`
	BackendStart   bool                          `json:"backend_starting,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

func (s *Session) Clone() *Session {
	b, _ := json.Marshal(s)
	var out Session
	_ = json.Unmarshal(b, &out)
	return &out
}

func (s *Session) AnyAttached() bool {
	for _, v := range s.Views {
		if v.Attached {
			return true
		}
	}
	return false
}

func (s *Session) AnyFocused() bool {
	for _, v := range s.Views {
		if v.Attached && v.Focused {
			return true
		}
	}
	return false
}

func (s *Session) SortedViews() []View {
	views := make([]View, 0, len(s.Views))
	for _, v := range s.Views {
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Endpoint == views[j].Endpoint {
			return views[i].WindowID < views[j].WindowID
		}
		return views[i].Endpoint < views[j].Endpoint
	})
	return views
}

type StateData struct {
	SchemaVersion int                 `json:"schema_version"`
	Sessions      map[string]*Session `json:"sessions"`
}

func newStateData() StateData {
	return StateData{SchemaVersion: stateSchemaVersion, Sessions: map[string]*Session{}}
}

type Snapshot struct {
	SchemaVersion int                `json:"schema_version"`
	Name          string             `json:"name"`
	CreatedAt     time.Time          `json:"created_at"`
	KittyVersion  string             `json:"kitty_version,omitempty"`
	Source        string             `json:"source"`
	NativeSession string             `json:"native_session,omitempty"`
	OSWindows     []SnapshotOSWindow `json:"os_windows"`
}

type SnapshotOSWindow struct {
	State   string        `json:"state,omitempty"`
	Class   string        `json:"class,omitempty"`
	Name    string        `json:"window_name,omitempty"`
	Focused bool          `json:"focused,omitempty"`
	Tabs    []SnapshotTab `json:"tabs"`
}

type SnapshotTab struct {
	Title       string          `json:"title"`
	Layout      string          `json:"layout"`
	Enabled     []string        `json:"enabled_layouts,omitempty"`
	LayoutState json.RawMessage `json:"layout_state,omitempty"`
	Active      bool            `json:"active,omitempty"`
	Views       []SnapshotView  `json:"views"`
}

type SnapshotView struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	CWD       string `json:"cwd,omitempty"`
	Active    bool   `json:"active,omitempty"`
}

type Event struct {
	SessionID string         `json:"session_id"`
	Kind      string         `json:"kind"`
	Source    string         `json:"source"`
	TurnID    string         `json:"turn_id,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	PID       int            `json:"pid,omitempty"`
	ExitCode  *int           `json:"exit_code,omitempty"`
	View      *View          `json:"view,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 80 {
		return fmt.Errorf("name must be at most 80 characters")
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return fmt.Errorf("name contains a control character")
	}
	return nil
}

func validateSnapshotName(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if filepath.Base(name) != name || name == "." || name == ".." || strings.Contains(name, "\\") {
		return fmt.Errorf("snapshot name must be a single filename component")
	}
	return nil
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
