package zka

import (
	"bytes"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterspersedWorkspaceFlagsMatchDocumentedSyntax(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pane := fs.String("pane", "", "")
	jsonOut := fs.Bool("json", false, "")
	if err := parseInterspersed(fs, []string{"devbox.example:example-project", "--pane", "abc", "--json"}); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "devbox.example:example-project" || *pane != "abc" || !*jsonOut {
		t.Fatalf("args=%#v pane=%q json=%v", fs.Args(), *pane, *jsonOut)
	}
}

func TestKittyPassthroughRejectsManagedProcessOptions(t *testing.T) {
	for _, args := range [][]string{{"--listen-on", "unix:/other"}, {"--session=x"}, {"--detach"}, {"--override", "shell=bash"}, {"bash"}} {
		if err := validateKittyPassthrough(args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
	if err := validateKittyPassthrough([]string{"--class", "managed", "--override", "font_size=12"}); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentIDIsStablePerNodeWorkspace(t *testing.T) {
	a := localAttachmentID("node", "workspace")
	if a != localAttachmentID("node", "workspace") || a == localAttachmentID("other", "workspace") {
		t.Fatalf("attachment ids are not deterministic: %q", a)
	}
}

func TestRunKittyReportsPrelaunchFailureWithoutPanic(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	t.Setenv("ZKA_KITTY_WATCHER", filepath.Join(t.TempDir(), "missing-watcher.py"))

	var stdout, stderr bytes.Buffer
	code, err := runKitty(nil, d.paths, &stdout, &stderr)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Kitty watcher not found") {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	if workspaces := d.listWorkspaces(); len(workspaces) != 0 {
		t.Fatalf("failed prelaunch retained workspaces: %#v", workspaces)
	}
}

func TestPreferredLocalAttachmentReusesReadyAlternateInstance(t *testing.T) {
	workspace := &Workspace{
		ID: "workspace", PrimaryAttachmentID: "stale",
		Attachments: map[string]*Attachment{
			"stale": {ID: "stale", Node: Host{ID: "node"}, Endpoint: "unix:/stale", Role: AttachmentPrimary, Status: AttachmentUnhealthy},
			"ready": {ID: "ready", Node: Host{ID: "node"}, Endpoint: "unix:/ready", Role: AttachmentMirror, Status: AttachmentReady},
		},
	}
	if got := preferredLocalAttachment(workspace, "node"); got == nil || got.ID != "ready" {
		t.Fatalf("preferred attachment = %#v", got)
	}
}
