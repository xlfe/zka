package zka

import (
	"context"
	"strings"
	"testing"
)

func TestKittyLaunchUsesStableVariablesAndNoShell(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, _ string, _ ...string) (string, string, error) {
		return "42\n", "", nil
	}}
	kitty := KittyClient{Runner: runner, Command: "kitten-test"}
	id, err := kitty.Launch(context.Background(), LaunchOptions{Endpoint: "unix:/kitty", Type: "tab", CWD: "/work dir", Title: "Reviewer", SessionID: "abc", Backend: "zmx", State: StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Fatalf("id = %d", id)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Name != "kitten-test" {
		t.Fatalf("calls = %#v", calls)
	}
	joined := strings.Join(calls[0].Args, "|")
	for _, want := range []string{"zka_session=abc", "zka_backend=zmx", "ZKA_SESSION_ID=abc", "/work dir"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launch args missing %q: %#v", want, calls[0].Args)
		}
	}
}

func TestFindManagedViews(t *testing.T) {
	tree := []kittyOSWindow{{IsFocused: true, Tabs: []kittyTab{{IsActive: true, Windows: []kittyWindow{{ID: 9, IsActive: true, UserVars: map[string]string{"zka_session": "abc"}}, {ID: 10, UserVars: map[string]string{}}}}}}}
	views := findManagedViews(tree)
	if len(views) != 1 || len(views["abc"]) != 1 || !views["abc"][0].Focused {
		t.Fatalf("views = %#v", views)
	}
}

func TestQuoteKitty(t *testing.T) {
	got := quoteKitty("a \"quote\" $HOME\nlaunch evil")
	if !strings.Contains(got, `$$HOME`) || !strings.Contains(got, ` launch evil`) || strings.ContainsRune(got, '\n') || !strings.HasPrefix(got, `"`) {
		t.Fatalf("quoted = %q", got)
	}
}

func TestKittyDirectiveIsUnquotedAndExpansionSafe(t *testing.T) {
	got := kittyDirective("$project\nnext")
	if got != "$$project next" {
		t.Fatalf("directive = %q", got)
	}
}
