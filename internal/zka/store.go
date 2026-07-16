package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type Store struct {
	paths Paths
}

func NewStore(paths Paths) *Store { return &Store{paths: paths} }

func (s *Store) Ensure() error {
	for _, dir := range []string{s.paths.StateDir, s.paths.GeneratedDir, s.paths.AttachmentDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure directory %s: %w", dir, err)
		}
	}
	return nil
}

// Load intentionally treats schema v1 as empty state. v1 represented
// user-visible process sessions and cannot be truthfully migrated into Kitty
// workspaces. NewDaemon immediately writes the replacement schema v2 state.
func (s *Store) Load() (StateData, error) {
	if err := s.Ensure(); err != nil {
		return StateData{}, err
	}
	b, err := os.ReadFile(s.paths.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return newStateData(), nil
	}
	if err != nil {
		return StateData{}, fmt.Errorf("read state: %w", err)
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &header); err != nil {
		return StateData{}, fmt.Errorf("decode state header: %w", err)
	}
	if header.SchemaVersion == 1 {
		if err := os.RemoveAll(filepath.Join(s.paths.StateDir, "snapshots")); err != nil {
			return StateData{}, fmt.Errorf("reset v1 snapshots: %w", err)
		}
		if err := os.RemoveAll(s.paths.GeneratedDir); err != nil {
			return StateData{}, fmt.Errorf("reset generated v1 files: %w", err)
		}
		if err := os.MkdirAll(s.paths.GeneratedDir, 0o700); err != nil {
			return StateData{}, fmt.Errorf("reset generated workspace files: %w", err)
		}
		return newStateData(), nil
	}
	if header.SchemaVersion != stateSchemaVersion {
		return StateData{}, fmt.Errorf("unsupported state schema %d (want %d)", header.SchemaVersion, stateSchemaVersion)
	}
	var state StateData
	if err := json.Unmarshal(b, &state); err != nil {
		return StateData{}, fmt.Errorf("decode state: %w", err)
	}
	if state.Workspaces == nil {
		state.Workspaces = map[string]*Workspace{}
	}
	if state.Remotes == nil {
		state.Remotes = map[string]*RemoteCache{}
	}
	for _, workspace := range state.Workspaces {
		normalizeWorkspace(workspace)
	}
	return state, nil
}

func normalizeWorkspace(workspace *Workspace) {
	if workspace.Panes == nil {
		workspace.Panes = map[string]*Pane{}
	}
	if workspace.Attachments == nil {
		workspace.Attachments = map[string]*Attachment{}
	}
	for _, pane := range workspace.Panes {
		if pane.Notifications == nil {
			pane.Notifications = map[string]NotificationRecord{}
		}
	}
	for _, attachment := range workspace.Attachments {
		if attachment.Views == nil {
			attachment.Views = map[string]RuntimeView{}
		}
		if attachment.ClientHeartbeats == nil {
			attachment.ClientHeartbeats = map[string]time.Time{}
		}
	}
	workspace.RecomputeAttention()
}

func (s *Store) Save(state StateData) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	state.SchemaVersion = stateSchemaVersion
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')
	return atomicWrite(s.paths.StateFile, b, 0o600)
}

func (s *Store) SessionPath(workspaceID, suffix string) string {
	name := shortID(workspaceID)
	if suffix != "" {
		name += "-" + suffix
	}
	return filepath.Join(s.paths.GeneratedDir, name+".kitty-session")
}

func (s *Store) WriteSession(workspaceID, suffix, content string) (string, error) {
	if err := s.Ensure(); err != nil {
		return "", err
	}
	path := s.SessionPath(workspaceID, suffix)
	if err := atomicWrite(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".zka-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ok = true
	return nil
}
