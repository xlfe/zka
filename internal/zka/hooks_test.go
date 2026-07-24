package zka

import (
	"bytes"
	"encoding/json"
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

func TestClaudeHookMapsInteractiveLifecycle(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	t.Setenv("ZKA_WORKSPACE_ID", workspace.ID)
	t.Setenv("ZKA_PANE_ID", pane.ID)

	tests := []struct {
		name       string
		input      string
		wantState  AgentState
		wantEvent  string
		wantDetail string
		wantTurn   string
		wantAgent  string
	}{
		{"start", `{"hook_event_name":"SessionStart","source":"startup"}`, StateIdle, "session_start", "startup", "", "claude"},
		{"prompt", `{"hook_event_name":"UserPromptSubmit","prompt_id":"prompt-1"}`, StateWorking, "user_prompt", "", "prompt-1", "claude"},
		{"question", `{"hook_event_name":"PreToolUse","prompt_id":"prompt-1","tool_name":"AskUserQuestion","tool_input":{"description":"choose a database"}}`, StateBlocked, "permission_request", "AskUserQuestion: choose a database", "prompt-1", "claude"},
		{"question answered", `{"hook_event_name":"PostToolUse","prompt_id":"prompt-1","tool_name":"AskUserQuestion"}`, StateWorking, "post_tool", "AskUserQuestion", "prompt-1", "claude"},
		{"plan approval", `{"hook_event_name":"PreToolUse","prompt_id":"prompt-1","tool_name":"ExitPlanMode"}`, StateBlocked, "permission_request", "ExitPlanMode", "prompt-1", "claude"},
		{"plan approved", `{"hook_event_name":"PostToolUse","prompt_id":"prompt-1","tool_name":"ExitPlanMode"}`, StateWorking, "post_tool", "ExitPlanMode", "prompt-1", "claude"},
		{"permission", `{"hook_event_name":"PermissionRequest","prompt_id":"prompt-1","tool_name":"Bash","tool_input":{"description":"run migrations"}}`, StateBlocked, "permission_request", "Bash: run migrations", "prompt-1", "claude"},
		{"permission notification", `{"hook_event_name":"Notification","prompt_id":"prompt-1","notification_type":"permission_prompt","message":"Claude needs permission"}`, StateBlocked, "permission_request", "Claude needs permission", "prompt-1", "claude"},
		{"idle notification", `{"hook_event_name":"Notification","prompt_id":"prompt-1","notification_type":"idle_prompt","message":"Claude is waiting"}`, StateDone, "stop", "Claude is waiting", "prompt-1", "claude"},
		{"stop", `{"hook_event_name":"Stop","prompt_id":"prompt-1","last_assistant_message":"Finished the migration work"}`, StateDone, "stop", "Finished the migration work", "prompt-1", "claude"},
		{"elicitation", `{"hook_event_name":"Elicitation","prompt_id":"prompt-2","mcp_server_name":"deploy","message":"Choose a region"}`, StateBlocked, "permission_request", "deploy: Choose a region", "prompt-2", "claude"},
		{"elicitation result", `{"hook_event_name":"ElicitationResult","prompt_id":"prompt-2","mcp_server_name":"deploy"}`, StateWorking, "post_tool", "deploy", "prompt-2", "claude"},
		{"tool failure", `{"hook_event_name":"PostToolUseFailure","prompt_id":"prompt-2","tool_name":"Bash","tool_input":{"description":"run tests"}}`, StateWorking, "post_tool", "Bash: run tests", "prompt-2", "claude"},
		{"elicitation notification", `{"hook_event_name":"Notification","prompt_id":"prompt-2","notification_type":"elicitation_dialog","message":"Input required"}`, StateBlocked, "permission_request", "Input required", "prompt-2", "claude"},
		{"elicitation completed", `{"hook_event_name":"Notification","prompt_id":"prompt-2","notification_type":"elicitation_complete","message":"Input received"}`, StateWorking, "post_tool", "Input received", "prompt-2", "claude"},
		{"api failure", `{"hook_event_name":"StopFailure","prompt_id":"prompt-2","error":"rate_limit","error_details":"429"}`, StateError, "agent_error", "rate_limit: 429", "prompt-2", "claude"},
		{"session end", `{"hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`, StateUnknown, "session_end", "prompt_input_exit", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			code, err := runHook([]string{"claude"}, d.paths, strings.NewReader(test.input), &output)
			if err != nil || code != 0 || output.String() != "{}\n" {
				t.Fatalf("runHook = %d, %v, %q", code, err, output.String())
			}
			got, err := d.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			reported := got.Panes[pane.ID]
			if reported.State != test.wantState || reported.Evidence.Event != test.wantEvent ||
				reported.Evidence.Detail != test.wantDetail || reported.LastTurnID != test.wantTurn ||
				reported.Agent != test.wantAgent {
				t.Fatalf("hook state = %#v", reported)
			}
			if test.wantEvent == "agent_error" && reported.BackendDead {
				t.Fatal("agent API failure marked the zmx backend dead")
			}
		})
	}
}

func TestClaudeHookSubagentsOnlyPropagateAttention(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	t.Setenv("ZKA_WORKSPACE_ID", workspace.ID)
	t.Setenv("ZKA_PANE_ID", pane.ID)

	run := func(input string) *Pane {
		t.Helper()
		var output bytes.Buffer
		code, err := runHook([]string{"claude"}, d.paths, strings.NewReader(input), &output)
		if err != nil || code != 0 || output.String() != "{}\n" {
			t.Fatalf("runHook = %d, %v, %q", code, err, output.String())
		}
		got, err := d.getWorkspace(workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Panes[pane.ID]
	}
	root := run(`{"hook_event_name":"UserPromptSubmit","prompt_id":"root"}`)
	if root.State != StateWorking {
		t.Fatalf("root state = %#v", root)
	}
	for _, input := range []string{
		`{"hook_event_name":"PostToolUse","prompt_id":"root","agent_id":"sub-1","tool_name":"Read"}`,
		`{"hook_event_name":"Stop","prompt_id":"root","agent_id":"sub-1"}`,
		`{"hook_event_name":"StopFailure","prompt_id":"root","agent_id":"sub-1","error":"server_error"}`,
	} {
		got := run(input)
		if got.State != StateWorking || got.Evidence.Event != "user_prompt" {
			t.Fatalf("subagent activity overwrote root state: %#v", got)
		}
	}
	blocked := run(`{"hook_event_name":"PermissionRequest","prompt_id":"root","agent_id":"sub-1","tool_name":"Bash"}`)
	if blocked.State != StateBlocked || blocked.Evidence.Event != "permission_request" {
		t.Fatalf("subagent permission was not propagated: %#v", blocked)
	}
	afterStop := run(`{"hook_event_name":"Stop","prompt_id":"root","agent_id":"sub-1"}`)
	if afterStop.State != StateBlocked || afterStop.Evidence.Event != "permission_request" {
		t.Fatalf("subagent completion cleared attention: %#v", afterStop)
	}
}

func TestClaudeHookIgnoresUnsupportedEventsAndTools(t *testing.T) {
	event, ok := mapHookEvent("claude", "workspace", "pane", agentHookInput{})
	if ok || event.Kind != "" {
		t.Fatalf("empty hook mapped: %#v", event)
	}
	for index, input := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Read"}`,
		`{"hook_event_name":"Notification","notification_type":"auth_success"}`,
		`{"hook_event_name":"SubagentStop","agent_id":"sub-1"}`,
	} {
		var parsed agentHookInput
		if err := json.Unmarshal([]byte(input), &parsed); err != nil {
			t.Fatal(err)
		}
		event, ok = mapHookEvent("claude", "workspace", "pane", parsed)
		if ok {
			t.Fatalf("case %d mapped to %s", index, event.Kind)
		}
	}
}

func TestManagedHookMalformedAndOversizedInputIsNoop(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	t.Setenv("ZKA_WORKSPACE_ID", workspace.ID)
	t.Setenv("ZKA_PANE_ID", pane.ID)

	inputs := []string{
		`{"hook_event_name":`,
		`{"hook_event_name":"UserPromptSubmit","message":"` + strings.Repeat("x", maxHookInputSize) + `"}`,
	}
	for _, input := range inputs {
		var output bytes.Buffer
		code, err := runHook([]string{"claude"}, d.paths, strings.NewReader(input), &output)
		if err != nil || code != 0 || output.String() != "{}\n" {
			t.Fatalf("runHook = %d, %v, %q", code, err, output.String())
		}
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reported := got.Panes[pane.ID]; reported.State != StateUnknown || reported.Agent != "" {
		t.Fatalf("invalid hook input changed pane: %#v", reported)
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

func TestHookRejectsUnknownAgent(t *testing.T) {
	var output bytes.Buffer
	code, err := runHook([]string{"other"}, testPaths(t.TempDir()), strings.NewReader(`{}`), &output)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "codex|claude") {
		t.Fatalf("runHook = %d, %v, %q", code, err, output.String())
	}
}
