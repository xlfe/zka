package launcher

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xlfe/zka/internal/zka"
)

func TestAttentionItemSummaryExplainsStateAgentOriginAgeAndEvidence(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	item := zka.AttentionItem{
		State: zka.StateBlocked, Agent: "codex", Origin: "devbox.example",
		TransitionedAt: now.Add(-17 * time.Minute), Detail: "approve command",
	}
	got := attentionItemSummary(item, now)
	for _, want := range []string{"Waiting for you", "codex", "devbox.example", "17m", "approve command"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q does not contain %q", got, want)
		}
	}
}

func TestAttentionAttachArgsTargetExactLocalOrRemotePane(t *testing.T) {
	for _, test := range []struct {
		name string
		item zka.AttentionItem
		want []string
	}{
		{
			name: "local",
			item: zka.AttentionItem{WorkspaceID: "workspace", PaneID: "pane"},
			want: []string{"workspace", "attach", "workspace", "--pane", "pane"},
		},
		{
			name: "remote",
			item: zka.AttentionItem{WorkspaceID: "workspace", PaneID: "pane", RemoteHost: "devbox.example"},
			want: []string{"workspace", "attach", "devbox.example:workspace", "--pane", "pane"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := attentionAttachArgs(test.item); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("args = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestAttentionHeadingKeepsMutedPendingCountWhilePaused(t *testing.T) {
	snapshot := zka.AttentionSnapshot{Paused: true, Counts: zka.AttentionCounts{Total: 4}}
	if got, want := attentionHeading(snapshot), "Attention paused · 4 pending"; got != want {
		t.Fatalf("heading = %q, want %q", got, want)
	}
}

func TestWaitingAgeUsesHumanScale(t *testing.T) {
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{30 * time.Second, "just now"},
		{9 * time.Minute, "9m"},
		{3 * time.Hour, "3h"},
		{49 * time.Hour, "2d"},
	} {
		if got := waitingAge(test.duration); got != test.want {
			t.Fatalf("waitingAge(%s) = %q, want %q", test.duration, got, test.want)
		}
	}
}
