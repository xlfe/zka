package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Store struct {
	paths Paths
}

func NewStore(paths Paths) *Store { return &Store{paths: paths} }

func (s *Store) Ensure() error {
	if err := os.MkdirAll(s.paths.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(s.paths.StateDir, 0o700); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	if err := os.MkdirAll(s.paths.SnapshotDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	return nil
}

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
	var state StateData
	if err := json.Unmarshal(b, &state); err != nil {
		return StateData{}, fmt.Errorf("decode state: %w", err)
	}
	if state.SchemaVersion != stateSchemaVersion {
		return StateData{}, fmt.Errorf("unsupported state schema %d (want %d)", state.SchemaVersion, stateSchemaVersion)
	}
	if state.Sessions == nil {
		state.Sessions = map[string]*Session{}
	}
	return state, nil
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

func (s *Store) SnapshotPath(name string) string {
	return filepath.Join(s.paths.SnapshotDir, name+".json")
}

func (s *Store) SaveSnapshot(snapshot Snapshot, output string) (string, error) {
	if err := validateSnapshotName(snapshot.Name); err != nil {
		return "", err
	}
	if err := s.Ensure(); err != nil {
		return "", err
	}
	if output == "" {
		output = s.SnapshotPath(snapshot.Name)
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode snapshot: %w", err)
	}
	b = append(b, '\n')
	if output == "-" {
		return string(b), nil
	}
	if err := atomicWrite(output, b, 0o600); err != nil {
		return "", err
	}
	return output, nil
}

func (s *Store) LoadSnapshot(nameOrPath string) (Snapshot, string, error) {
	path := nameOrPath
	if filepath.Ext(path) == "" && filepath.Base(path) == path {
		path = s.SnapshotPath(path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, path, fmt.Errorf("read snapshot: %w", err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return Snapshot{}, path, fmt.Errorf("decode snapshot: %w", err)
	}
	if snapshot.SchemaVersion != snapshotSchemaVersion {
		return Snapshot{}, path, fmt.Errorf("unsupported snapshot schema %d", snapshot.SchemaVersion)
	}
	return snapshot, path, nil
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
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ok = true
	return nil
}
