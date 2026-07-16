package zka

import (
	"testing"
	"time"
)

func TestAgentStateAndWorkspaceAggregate(t *testing.T) {
	for _, state := range []AgentState{StateUnknown, StateIdle, StateWorking, StateBlocked, StateDone, StateError} {
		if !state.Valid() {
			t.Fatalf("expected %q to be valid", state)
		}
	}
	workspace := &Workspace{Panes: map[string]*Pane{
		"idle": {State: StateIdle}, "working": {State: StateWorking}, "blocked": {State: StateBlocked},
	}}
	if got := workspace.RecomputeAttention(); got != StateBlocked {
		t.Fatalf("aggregate = %s", got)
	}
	workspace.Panes["error"] = &Pane{State: StateError}
	if got := workspace.RecomputeAttention(); got != StateError {
		t.Fatalf("aggregate = %s", got)
	}
	if AgentState("waiting").Valid() {
		t.Fatal("custom state accepted")
	}
}

func TestBackendNameIsPerPane(t *testing.T) {
	got := backendName("0123456789abcdef", "fedcba9876543210")
	if got != "zka-01234567-fedcba98" {
		t.Fatalf("backendName = %q", got)
	}
}

func TestValidateWorkspaceName(t *testing.T) {
	if err := validateName("example-project"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "bad\nname", "host:name"} {
		if err := validateName(value); err == nil {
			t.Fatalf("validateName(%q) succeeded", value)
		}
	}
}

func TestSortedPanesUsesPosition(t *testing.T) {
	now := time.Now()
	workspace := &Workspace{Panes: map[string]*Pane{
		"b": {ID: "b", Position: 1, CreatedAt: now},
		"a": {ID: "a", Position: 0, CreatedAt: now},
	}}
	panes := workspace.SortedPanes()
	if panes[0].ID != "a" || panes[1].ID != "b" {
		t.Fatalf("pane order = %#v", panes)
	}
}
