package zka

import (
	"os"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	paths := testPaths(t.TempDir())
	store := NewStore(paths)
	state := newStateData()
	state.Sessions["abc"] = &Session{ID: "abc", Name: "test", State: StateIdle, CreatedAt: time.Now(), UpdatedAt: time.Now()}
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
	if loaded.Sessions["abc"].Name != "test" {
		t.Fatalf("unexpected loaded state: %#v", loaded)
	}
}

func TestStoreRejectsUnknownSchema(t *testing.T) {
	paths := testPaths(t.TempDir())
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, []byte(`{"schema_version":99,"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(paths).Load(); err == nil {
		t.Fatal("unknown schema was accepted")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	paths := testPaths(t.TempDir())
	store := NewStore(paths)
	snapshot := Snapshot{SchemaVersion: snapshotSchemaVersion, Name: "daily", CreatedAt: time.Now(), OSWindows: []SnapshotOSWindow{{Tabs: []SnapshotTab{{Views: []SnapshotView{{SessionID: "abc"}}}}}}}
	path, err := store.SaveSnapshot(snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	loaded, gotPath, err := store.LoadSnapshot("daily")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path || loaded.Name != "daily" {
		t.Fatalf("round trip = %#v, %q", loaded, gotPath)
	}
}
