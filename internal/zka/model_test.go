package zka

import "testing"

func TestAgentStateValid(t *testing.T) {
	for _, state := range []AgentState{StateUnknown, StateIdle, StateWorking, StateBlocked, StateDone, StateError} {
		if !state.Valid() {
			t.Fatalf("expected %q to be valid", state)
		}
	}
	if AgentState("waiting").Valid() {
		t.Fatal("unexpected custom state accepted")
	}
}

func TestBackendName(t *testing.T) {
	got := backendName("Review This!", "0123456789abcdef")
	if got != "zka-review-this-01234567" {
		t.Fatalf("backendName = %q", got)
	}
}

func TestValidateName(t *testing.T) {
	if err := validateName("reviewer"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "bad\nname"} {
		if err := validateName(value); err == nil {
			t.Fatalf("validateName(%q) succeeded", value)
		}
	}
}

func TestValidateSnapshotNameRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../daily", "dir/daily", `dir\daily`} {
		if err := validateSnapshotName(value); err == nil {
			t.Fatalf("validateSnapshotName(%q) succeeded", value)
		}
	}
}
