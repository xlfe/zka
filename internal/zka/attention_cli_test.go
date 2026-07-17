package zka

import (
	"bytes"
	"strings"
	"testing"
)

func TestAttentionWaybarStates(t *testing.T) {
	tests := []struct {
		name        string
		snapshot    AttentionSnapshot
		unavailable error
		text        string
		class       string
	}{
		{name: "clear", snapshot: AttentionSnapshot{Highest: StateIdle}, text: "0", class: "clear"},
		{name: "blocked", snapshot: AttentionSnapshot{Highest: StateBlocked, Counts: AttentionCounts{Total: 2}}, text: "2", class: "blocked"},
		{name: "paused", snapshot: AttentionSnapshot{Paused: true, Counts: AttentionCounts{Total: 3}}, text: "3", class: "paused"},
		{name: "unavailable", unavailable: errTestUnavailable{}, text: "?", class: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := attentionWaybar(test.snapshot, test.unavailable)
			if got.Text != test.text || got.Class != test.class {
				t.Fatalf("waybar = %#v", got)
			}
		})
	}
}

type errTestUnavailable struct{}

func (errTestUnavailable) Error() string { return "offline" }

func TestAttentionWaybarTooltipIncludesActionableRows(t *testing.T) {
	snapshot := AttentionSnapshot{
		Highest: StateBlocked,
		Counts:  AttentionCounts{Total: 1, Blocked: 1},
		Items:   []AttentionItem{{WorkspaceName: "example-project", PaneTitle: "tests", Agent: "codex", State: StateBlocked, Detail: "approve command"}},
	}
	var output bytes.Buffer
	if err := writeAttentionOutput(&output, attentionOutputWaybar, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"text":"1"`, `"class":"blocked"`, "Waiting for you", "approve command"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q does not contain %q", output.String(), want)
		}
	}
}
