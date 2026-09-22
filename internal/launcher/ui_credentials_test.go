package launcher

import (
	"context"
	"errors"
	"image"
	"reflect"
	"strings"
	"testing"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/xlfe/zka/internal/zka"
)

type credentialLauncherBackend struct {
	recordingLauncherBackend
	snapshots chan []*zka.Workspace
	requests  chan string
}

func (b *credentialLauncherBackend) Node(context.Context) (zka.Host, error) {
	return zka.Host{ID: "origin", Name: "desktop"}, nil
}

func (b *credentialLauncherBackend) Workspaces(ctx context.Context, host string) ([]*zka.Workspace, error) {
	b.requests <- host
	select {
	case snapshot := <-b.snapshots:
		return snapshot, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func credentialLauncher(t *testing.T) (*ui, *credentialLauncherBackend, *zka.Workspace) {
	t.Helper()
	backend := &credentialLauncherBackend{
		recordingLauncherBackend: recordingLauncherBackend{executed: make(chan []string, 4)},
		snapshots:                make(chan []*zka.Workspace, 1), requests: make(chan string, 4),
	}
	workspace := &zka.Workspace{
		ID: "workspace", Name: "shell",
		CredentialClaim: &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "laptop", ProviderSource: "remote"},
		Attachments: map[string]*zka.Attachment{
			"local":  {ID: "local", Node: zka.Host{ID: "origin"}, Endpoint: "unix:/view", Status: zka.AttachmentReady},
			"remote": {ID: "remote", Node: zka.Host{ID: "laptop", Name: "laptop"}, Status: zka.AttachmentReady},
		},
	}
	ui := newUI(backend)
	ui.ctx, ui.cancel = context.WithCancel(context.Background())
	t.Cleanup(ui.cancel)
	ui.window = new(app.Window)
	ui.localNodeID = "origin"
	ui.local = []*zka.Workspace{workspace}
	ui.selected = 2
	ui.credentialsEnabled = true
	ui.defaultBundle = "default"
	ui.credentialBundles = map[string]bool{"work": true, "default": true}
	return ui, backend, workspace
}

func drainLauncherResult(t *testing.T, ui *ui, kind resultKind) {
	t.Helper()
	select {
	case result := <-ui.results:
		if result.kind != kind {
			t.Fatalf("result kind = %d, want %d", result.kind, kind)
		}
		ui.results <- result
		ui.drainResults()
	case <-time.After(time.Second):
		t.Fatal("launcher operation did not finish")
	}
}

func TestOriginPopupReclaimsCredentialsAndRefreshesOwnership(t *testing.T) {
	ui, backend, workspace := credentialLauncher(t)
	if label := workspaceCredentialButtonLabel(workspace, ui.localNodeID); label != "Claim credentials here" {
		t.Fatalf("remote claim button = %q", label)
	}
	ui.toggleCredentialSelection()
	if !ui.busy || ui.operationKind != resultCredentials {
		t.Fatal("reclaim did not start a credential operation")
	}
	select {
	case args := <-backend.executed:
		want := []string{"workspace", "credentials", "activate-local", "workspace", "--bundle", "work"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("reclaim args = %#v, want %#v", args, want)
		}
	case <-time.After(time.Second):
		t.Fatal("reclaim command did not execute")
	}
	drainLauncherResult(t, ui, resultCredentials)
	if !ui.localLoading || ui.busy {
		t.Fatal("successful reclaim did not reload ownership")
	}
	updated := *workspace
	updated.CredentialClaim = &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "origin", ProviderSource: "local"}
	backend.snapshots <- []*zka.Workspace{&updated}
	drainLauncherResult(t, ui, resultLocal)
	if label := workspaceCredentialButtonLabel(ui.local[0], ui.localNodeID); label != "Release credentials" {
		t.Fatalf("reclaimed button = %q", label)
	}
	if summary := workspaceCredentialSummary(ui.local[0], ui.localNodeID); !strings.Contains(summary, "claimed on this machine") {
		t.Fatalf("reclaimed summary = %q", summary)
	}
	if ui.screen != screenHome || ui.selected != 2 || ui.errorMessage != "" {
		t.Fatalf("reclaim left screen=%d selection=%d error=%q", ui.screen, ui.selected, ui.errorMessage)
	}
	if len(backend.executed) != 0 {
		t.Fatal("reclaim issued an extra command")
	}
}

func TestOriginPopupFailedReclaimRetainsRemoteOwner(t *testing.T) {
	ui, backend, workspace := credentialLauncher(t)
	backend.executeErr = errors.New("local card unavailable")
	ui.toggleCredentialSelection()
	drainLauncherResult(t, ui, resultCredentials)
	if ui.errorMessage != "local card unavailable" || ui.busy || ui.local[0] != workspace {
		t.Fatalf("failed reclaim changed ownership or lost the error: %q", ui.errorMessage)
	}
	if len(backend.requests) != 0 {
		t.Fatal("failed reclaim pretended ownership had changed")
	}
}

func TestPopupRefreshObservesRemoteClaimWithoutChangingSelection(t *testing.T) {
	ui, backend, remoteClaim := credentialLauncher(t)
	localClaim := *remoteClaim
	localClaim.CredentialClaim = &zka.CredentialClaim{Bundle: "work", OwnerNodeID: "origin"}
	ui.local = []*zka.Workspace{&localClaim}
	ui.nextLocalRefresh = time.Now().Add(-time.Second)
	ui.refreshLocal(time.Now())
	if !ui.localLoading || ui.local[0] != &localClaim {
		t.Fatal("background refresh hid the current workspace")
	}
	// A second frame while loading must not launch an overlapping request.
	ui.refreshLocal(time.Now().Add(time.Minute))
	other := &zka.Workspace{ID: "another", Name: "aaa", Attachments: remoteClaim.Attachments}
	backend.snapshots <- []*zka.Workspace{other, remoteClaim}
	drainLauncherResult(t, ui, resultLocal)
	if ui.selected != 3 || ui.local[ui.selected-2].ID != remoteClaim.ID {
		t.Fatalf("refresh moved the selected workspace: index=%d", ui.selected)
	}
	if len(backend.requests) != 1 || <-backend.requests != "" || len(backend.executed) != 0 {
		t.Fatal("refresh must only read one local workspace snapshot")
	}
	gtx := layout.Context{
		Ops: new(op.Ops), Constraints: layout.Exact(image.Pt(680, 560)),
		Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	ui.layoutHome(gtx)
	label := ui.selectables["workspace::workspace:credentials"]
	if label == nil {
		t.Fatal("ownership was not rendered as a dedicated line")
	}
	want := "Credentials: claimed remotely by laptop · work"
	label.SetCaret(0, len([]rune(want)))
	if got := label.SelectedText(); got != want {
		t.Fatalf("rendered ownership = %q, want %q", got, want)
	}
}

func TestPopupRefreshWaitsForUserOperations(t *testing.T) {
	ui, backend, _ := credentialLauncher(t)
	ui.busy = true
	ui.refreshLocal(time.Now())
	ui.busy = false
	ui.screen = screenRemoteList
	ui.refreshLocal(time.Now())
	ui.screen = screenHome
	ui.nextLocalRefresh = time.Now().Add(time.Minute)
	ui.refreshLocal(time.Now())
	if ui.localLoading || len(backend.requests) != 0 {
		t.Fatal("refresh ran during an operation, remote screen or refresh interval")
	}
}

func TestLocalCredentialActionsIncludeUnclaimedAndConfiguredBundles(t *testing.T) {
	ui, _, workspace := credentialLauncher(t)
	ui.defaultBundle = ""
	if !ui.workspaceCredentialControlVisible(workspace) {
		t.Fatal("configured existing bundle should be reclaimable without a default")
	}
	workspace.CredentialClaim = nil
	if summary := workspaceCredentialSummary(workspace, ui.localNodeID); summary != "Credentials: unclaimed" {
		t.Fatalf("unclaimed local summary = %q", summary)
	}
	if ui.workspaceCredentialControlVisible(workspace) {
		t.Fatal("unclaimed workspace offered activation without a bundle")
	}
	ui.defaultBundle = "work"
	if !ui.workspaceCredentialControlVisible(workspace) {
		t.Fatal("unclaimed local workspace did not offer activation")
	}
	args, _ := workspaceCredentialAction(workspace, ui.localNodeID)
	if !reflect.DeepEqual(args, []string{"workspace", "credentials", "activate-local", "workspace"}) {
		t.Fatalf("unclaimed local action = %#v", args)
	}
}
