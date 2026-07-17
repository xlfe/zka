package zka

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	paths := testPaths(t.TempDir())
	store := NewStore(paths)
	state := newStateData()
	state.Node = Host{ID: "node", Name: "devbox.example"}
	state.AttentionPaused = true
	state.Workspaces["abc"] = &Workspace{
		ID: "abc", Name: "test", Origin: state.Node, Revision: 1,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspaces["abc"].Name != "test" || loaded.Node.Name != "devbox.example" || !loaded.AttentionPaused {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestStoreResetsUndeployedV1Schema(t *testing.T) {
	paths := testPaths(t.TempDir())
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"schema_version":1,"sessions":{"old":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != stateSchemaVersion || len(state.Workspaces) != 0 {
		t.Fatalf("state = %#v", state)
	}
}

func TestStoreResetsV2StateAndGeneratedFiles(t *testing.T) {
	paths := testPaths(t.TempDir())
	if err := os.MkdirAll(paths.GeneratedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"schema_version":2,"workspaces":{"old":{"id":"old"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(paths.GeneratedDir, "old.kitty-session")
	if err := os.WriteFile(generated, []byte("launch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != 3 || len(state.Workspaces) != 0 {
		t.Fatalf("state = %#v", state)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("legacy generated session remains: %v", err)
	}
}

func TestStoreRejectsFutureSchema(t *testing.T) {
	paths := testPaths(t.TempDir())
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"schema_version":99,"workspaces":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(paths).Load(); err == nil {
		t.Fatal("future schema was accepted")
	}
}

func TestGeneratedSessionIsAtomicAndPrivate(t *testing.T) {
	paths := testPaths(t.TempDir())
	path, err := NewStore(paths).WriteSession("0123456789", "attachment", "launch\n")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode = %o", info.Mode().Perm())
	}
}
