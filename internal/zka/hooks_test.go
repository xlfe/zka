package zka

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestCodexHookMapsUserPrompt(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx) }()
	waitFor(t, func() bool {
		_, err := os.Stat(d.paths.Socket)
		return err == nil
	})
	session := createTestSession(t, d)
	t.Setenv("ZKA_SESSION_ID", session.ID)
	input := `{"session_id":"codex-id","turn_id":"turn-7","hook_event_name":"UserPromptSubmit"}`
	var output bytes.Buffer
	code, err := runHook([]string{"codex"}, d.paths, strings.NewReader(input), &output)
	if err != nil || code != 0 {
		t.Fatalf("runHook = %d, %v", code, err)
	}
	if output.String() != "{}\n" {
		t.Fatalf("unexpected hook output %q", output.String())
	}
	got, err := d.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateWorking || got.LastTurnID != "turn-7" || got.Evidence.Source != "codex-hook" {
		t.Fatalf("hook state = %#v", got)
	}
}

func TestCodexHookWithoutZKASessionIsNoop(t *testing.T) {
	t.Setenv("ZKA_SESSION_ID", "")
	var output bytes.Buffer
	code, err := runHook([]string{"codex"}, testPaths(t.TempDir()), strings.NewReader(`{"hook_event_name":"Stop"}`), &output)
	if err != nil || code != 0 {
		t.Fatalf("runHook = %d, %v", code, err)
	}
	if output.String() != "{}\n" {
		t.Fatalf("unexpected hook output %q", output.String())
	}
}

func TestManagedHookDoesNotRequireRuntimeForUnmanagedSession(t *testing.T) {
	t.Setenv("ZKA_SESSION_ID", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("ZKA_RUNTIME_DIR", "")
	var stdout, stderr bytes.Buffer
	code, err := Run([]string{"hook", "codex"}, strings.NewReader(`{"hook_event_name":"Stop"}`), &stdout, &stderr)
	if err != nil || code != 0 || stdout.String() != "{}\n" || stderr.Len() != 0 {
		t.Fatalf("Run hook = code %d, err %v, stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
	}
}
