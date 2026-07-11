package zka

import (
	"path/filepath"
	"testing"
)

func TestDefaultPathsUseXDGDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-home"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime-home"))
	t.Setenv("ZKA_STATE_DIR", "")
	t.Setenv("ZKA_RUNTIME_DIR", "")
	t.Setenv("ZKA_SOCKET", "")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.StateDir != filepath.Join(root, "state-home", "zka") || paths.Socket != filepath.Join(root, "runtime-home", "zka", "zka.sock") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestDefaultPathsRejectRelativeOverride(t *testing.T) {
	t.Setenv("ZKA_STATE_DIR", "relative")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if _, err := DefaultPaths(); err == nil {
		t.Fatal("relative state directory accepted")
	}
}
