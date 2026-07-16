package zka

import (
	"context"
	"strings"
	"testing"
	"time"
)

func templateWorkspace() *Workspace {
	return &Workspace{
		ID: "workspace", Name: "example-project", Revision: 1,
		Panes: map[string]*Pane{
			"pane-a": {ID: "pane-a", Position: 0, CWD: "/one", Title: "One", Visible: true, State: StateIdle, CreatedAt: time.Unix(1, 0)},
			"pane-b": {ID: "pane-b", Position: 1, CWD: "/two", Title: "Two", Visible: true, State: StateDone, CreatedAt: time.Unix(2, 0)},
		},
		Attachments: map[string]*Attachment{},
	}
}

func TestTopologyTemplateRejectsProgramsAndReservedVariables(t *testing.T) {
	for _, content := range []string{
		"launch --cwd /work codex\n",
		"launch --var zka_pane=mine\n",
		"launch --var zka_state=done\n",
		"launch --var zka_ready=1\n",
		"launch --env ZKA_WORKSPACE_ID=mine\n",
		"launch --watcher /tmp/code.py\n",
		"launch --copy-cmdline\n",
		"map ctrl+x launch evil\n",
	} {
		if _, err := ParseSessionTemplate(content); err == nil {
			t.Fatalf("unsafe template accepted: %q", content)
		}
	}
}

func TestGenerateManagedSessionUsesOneHiddenBackendPerLaunch(t *testing.T) {
	template, err := ParseSessionTemplate("new_tab agents\nlayout splits\nlaunch --cwd /one\nlaunch --cwd /two --location vsplit\nfocus\n")
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateManagedSession(template, templateWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range []string{"pane-a", "pane-b"} {
		if !strings.Contains(got, "zka_pane="+pane) || !strings.Contains(got, "zka_ready=0") || !strings.Contains(got, "zka pane --workspace workspace --pane "+pane) {
			t.Fatalf("generated session missing %s:\n%s", pane, got)
		}
	}
	if strings.Contains(got, "codex") || strings.Contains(got, "nvim") {
		t.Fatalf("foreground command leaked:\n%s", got)
	}
}

func TestCanonicalizerNeverRestartsForegroundProgram(t *testing.T) {
	workspace := templateWorkspace()
	delete(workspace.Panes, "pane-b")
	native := "new_tab agents\nlayout splits\nset_layout_state {\"bias\":40}\nlaunch --cwd /work --title '[!] Agent' --var zka_workspace=workspace --var zka_pane=pane-a --env FOO=bar codex --resume secret\nfocus\n"
	got, err := CanonicalizeKittySession(native, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"codex", "--resume", "secret", "FOO=bar"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("canonical output contains %q:\n%s", forbidden, got)
		}
	}
	if strings.Contains(got, "[!]") {
		t.Fatalf("attention marker leaked into manifest:\n%s", got)
	}
	for _, required := range []string{"set_layout_state", "zka pane --workspace workspace --pane pane-a"} {
		if !strings.Contains(got, required) {
			t.Fatalf("canonical output missing %q:\n%s", required, got)
		}
	}
}

func TestRenderRemoteAttachmentUsesSSHViewWrapperOnly(t *testing.T) {
	workspace := templateWorkspace()
	template, _ := ParseSessionTemplate("launch\nlaunch --location vsplit\n")
	local, err := GenerateManagedSession(template, workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspace.Manifest.Session = "cd /one\n" + local
	remote, err := RenderAttachmentSession(workspace, Transport{Kind: "ssh", Host: "devbox.example"}, "attachment")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, "zka remote-pane --origin devbox.example") || strings.Contains(remote, "zmx attach") || strings.Contains(remote, "zka pane --workspace") || strings.Contains(remote, "--cwd") || strings.Contains(remote, "cd /one") {
		t.Fatalf("remote session:\n%s", remote)
	}
}

func TestRemoteCaptureKeepsOriginCWDInCanonicalManifest(t *testing.T) {
	workspace := templateWorkspace()
	delete(workspace.Panes, "pane-b")
	workspace.RemoteHost = "devbox.example"
	native := "launch --cwd /destination --var zka_workspace=workspace --var zka_pane=pane-a --var zka_ready=1 zka remote-pane\n"
	got, err := CanonicalizeKittySession(native, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/one") || strings.Contains(got, "/destination") {
		t.Fatalf("remote canonical CWD:\n%s", got)
	}
}

func TestCaptureManifestRequiresEveryDedicatedKittyWindowTagged(t *testing.T) {
	workspace := templateWorkspace()
	delete(workspace.Panes, "pane-b")
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "--version":
			return "kitten 0.47.4\n", "", nil
		case strings.Contains(joined, "--output-format=session"):
			return "new_tab work\nlayout splits\nlaunch --var zka_workspace=workspace --var zka_pane=pane-a codex\n", "", nil
		default:
			return `[{"id":1,"tabs":[{"id":2,"title":"work","layout":"splits","windows":[{"id":3,"title":"agent","cwd":"/work","user_vars":{"zka_workspace":"workspace","zka_pane":"pane-a","zka_ready":"1"}}]}]}]`, "", nil
		}
	}}
	manifest, views, err := CaptureManifest(context.Background(), KittyClient{Runner: runner}, "unix:/kitty", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.KittyVersion != "kitten 0.47.4" || len(views) != 1 || !views["pane-a"].Ready || strings.Contains(manifest.Session, "codex") {
		t.Fatalf("manifest=%#v views=%#v", manifest, views)
	}
	runner.handler = func(_ context.Context, _ string, _ ...string) (string, string, error) {
		return `[{"tabs":[{"windows":[{"id":4,"user_vars":{}}]}]}]`, "", nil
	}
	if _, _, err := CaptureManifest(context.Background(), KittyClient{Runner: runner}, "unix:/kitty", workspace); err == nil || !strings.Contains(err.Error(), "untagged") {
		t.Fatalf("untagged error = %v", err)
	}
}

func TestSessionWordParserDoesNotEvaluateShell(t *testing.T) {
	tokens, err := splitSessionWords(`launch --title "$(touch nope)" --cwd '$HOME'`)
	if err != nil {
		t.Fatal(err)
	}
	if tokens[2] != "$(touch nope)" || tokens[4] != "$HOME" {
		t.Fatalf("tokens = %#v", tokens)
	}
}
