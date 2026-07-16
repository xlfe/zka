package zka

import (
	"bytes"
	"strings"
	"testing"
)

func TestCodexHookMapsUserPromptToHiddenPane(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	t.Setenv("ZKA_WORKSPACE_ID", workspace.ID)
	t.Setenv("ZKA_PANE_ID", pane.ID)
	input := `{"session_id":"codex-id","turn_id":"turn-7","hook_event_name":"UserPromptSubmit"}`
	var output bytes.Buffer
	code, err := runHook([]string{"codex"}, d.paths, strings.NewReader(input), &output)
	if err != nil || code != 0 {
		t.Fatalf("runHook = %d, %v", code, err)
	}
	if output.String() != "{}\n" {
		t.Fatalf("hook output = %q", output.String())
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	reported := got.Panes[pane.ID]
	if reported.State != StateWorking || reported.LastTurnID != "turn-7" || reported.Evidence.Source != "codex-hook" {
		t.Fatalf("hook state = %#v", reported)
	}
}

func TestCodexHookWithoutManagedPaneIsNoop(t *testing.T) {
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")
	var output bytes.Buffer
	code, err := runHook([]string{"codex"}, testPaths(t.TempDir()), strings.NewReader(`{"hook_event_name":"Stop"}`), &output)
	if err != nil || code != 0 || output.String() != "{}\n" {
		t.Fatalf("hook = %d, %v, %q", code, err, output.String())
	}
}

func TestManagedHookDoesNotRequireRuntimeForUnmanagedShell(t *testing.T) {
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_PANE_ID", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("ZKA_RUNTIME_DIR", "")
	var stdout, stderr bytes.Buffer
	code, err := Run([]string{"hook", "codex"}, strings.NewReader(`{"hook_event_name":"Stop"}`), &stdout, &stderr)
	if err != nil || code != 0 || stdout.String() != "{}\n" || stderr.Len() != 0 {
		t.Fatalf("Run hook = %d, %v, %q, %q", code, err, stdout.String(), stderr.String())
	}
}
