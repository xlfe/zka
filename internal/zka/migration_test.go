package zka

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSchemaSixMigrationWritesRollbackBackup(t *testing.T) {
	paths := testPaths(testRoot(t))
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := newStateData()
	state.SchemaVersion = 6
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != stateSchemaVersion {
		t.Fatalf("schema = %d, want %d", loaded.SchemaVersion, stateSchemaVersion)
	}
	backup, err := os.ReadFile(paths.StateFile + ".v6.backup")
	if err != nil || string(backup) != string(encoded) {
		t.Fatalf("v6 migration backup missing or wrong: %v", err)
	}
}

func TestSchemaEightCredentialClaimsMigrateToNodeOwnership(t *testing.T) {
	paths := testPaths(testRoot(t))
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := newStateData()
	state.SchemaVersion = 8
	state.Node = Host{ID: "origin", Name: "origin"}
	state.Workspaces["remote"] = &Workspace{
		ID: "remote", Name: "remote", Origin: state.Node,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
		CredentialClaim: &CredentialClaim{
			Bundle: "work", OwnerNodeID: "provider", OwnerAttachmentID: "view", Generation: 4, State: "ready",
			Capabilities: map[string]CredentialCapabilityStatus{credentialCapabilitySSH: {State: "ready", Available: true}},
		},
		PIVBProvider: &WorkspacePIVBProvider{
			Source: "attachment", Bundle: "work", OwnerNodeID: "provider", OwnerAttachmentID: "view", Generation: 4, State: "ready",
			Manifest: CredentialPIVBManifest{ProtocolVersion: pivbForwardProtocolVersion, Card: CredentialPIVBCard{Serial: 7, KeyID: "kid"}},
		},
	}
	state.Workspaces["local"] = &Workspace{
		ID: "local", Name: "local", Origin: state.Node,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
		PIVBProvider: &WorkspacePIVBProvider{
			Source: "local", Bundle: "work", OwnerNodeID: "origin", Generation: 2, State: "ready",
			Manifest: CredentialPIVBManifest{ProtocolVersion: pivbForwardProtocolVersion, Card: CredentialPIVBCard{Serial: 8, KeyID: "local-kid"}},
		},
	}
	state.Remotes["devbox"] = &RemoteCache{Host: "devbox", Workspaces: map[string]*Workspace{
		"remote": state.Workspaces["remote"].Clone(),
	}}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	remote := loaded.Workspaces["remote"]
	if remote.CredentialClaim.ProviderSource != "remote" || remote.CredentialClaim.OwnerAttachmentID != "" ||
		remote.CredentialClaim.PIVB == nil || remote.PIVBProvider != nil || remote.CredentialGeneration != 4 {
		t.Fatalf("remote v8 binding = %#v, legacy=%#v", remote.CredentialClaim, remote.PIVBProvider)
	}
	local := loaded.Workspaces["local"]
	if local.CredentialClaim == nil || local.CredentialClaim.ProviderSource != "local" || local.CredentialClaim.OwnerNodeID != "origin" || local.PIVBProvider != nil || local.CredentialGeneration != 2 {
		t.Fatalf("local v8 binding = %#v, legacy=%#v", local.CredentialClaim, local.PIVBProvider)
	}
	cached := loaded.Remotes["devbox"].Workspaces["remote"]
	if cached.CredentialClaim.ProviderSource != "remote" || cached.CredentialClaim.OwnerAttachmentID != "" || cached.CredentialClaim.PIVB == nil || cached.CredentialGeneration != 4 {
		t.Fatalf("cached remote v8 binding = %#v", cached.CredentialClaim)
	}
	if backup, err := os.ReadFile(paths.StateFile + ".v8.backup"); err != nil || string(backup) != string(encoded) {
		t.Fatalf("v8 migration backup missing or wrong: %v", err)
	}
}

func TestSchemaEightSplitProviderConflictFailsClosedAndRemainsInspectable(t *testing.T) {
	paths := testPaths(testRoot(t))
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := newStateData()
	state.SchemaVersion = 8
	state.Node = Host{ID: "origin", Name: "origin"}
	state.Workspaces["conflict"] = &Workspace{
		ID: "conflict", Name: "conflict", Origin: state.Node,
		Panes: map[string]*Pane{}, Attachments: map[string]*Attachment{},
		CredentialClaim: &CredentialClaim{
			Bundle: "work", OwnerNodeID: "remote-provider", Generation: 4, State: "ready",
			Capabilities: map[string]CredentialCapabilityStatus{credentialCapabilitySSH: {State: "ready", Available: true}},
		},
		PIVBProvider: &WorkspacePIVBProvider{
			Source: "local", Bundle: "work", OwnerNodeID: "origin", Generation: 5, State: "ready",
			Manifest: CredentialPIVBManifest{Card: CredentialPIVBCard{Serial: 42, KeyID: "legacy-key"}},
		},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	workspace := loaded.Workspaces["conflict"]
	if workspace.CredentialClaim == nil || workspace.CredentialClaim.State != "migration_conflict" || workspace.PIVBProvider == nil {
		t.Fatalf("split-provider migration = claim=%#v legacy=%#v", workspace.CredentialClaim, workspace.PIVBProvider)
	}
	for _, capability := range workspace.CredentialClaim.Capabilities {
		if capability.Available || capability.State != "unavailable" {
			t.Fatalf("conflicting capability remained available: %#v", capability)
		}
	}
	if workspace.CredentialGeneration != 5 {
		t.Fatalf("credential generation = %d, want 5", workspace.CredentialGeneration)
	}
	var detail strings.Builder
	writeWorkspaceDetail(&detail, workspace)
	if output := detail.String(); !strings.Contains(output, "credential_migration_conflict=legacy_pivb") || !strings.Contains(output, "serial=42") || !strings.Contains(output, "key=legacy-key") {
		t.Fatalf("inspect detail omitted retained legacy provider: %s", output)
	}
}

// wedgedV5State reproduces the shape of the real outage: schema 5, a workspace
// whose desired topology contains a fabricated "Recovered" tab with no
// enabled_layouts and no layout_state, and an attachment stuck one generation
// behind because that tab could never be reproduced.
func wedgedV5State() StateData {
	now := time.Now().UTC()
	pane := func(id, title string) *Pane {
		return &Pane{
			ID: id, Title: title, State: StateUnknown,
			Backend:   BackendRef{Kind: "zmx", Ref: "zka-ws-" + id},
			CreatedAt: now, UpdatedAt: now,
		}
	}
	roots := []Node{{
		ID: "os-1", Kind: "os-window", Class: "managed-kitty",
		Children: []Node{
			{
				ID: "tab-1", Kind: "tab", Title: "shell", Layout: "fat",
				EnabledLayouts: []string{"fat", "splits", "tall"},
				LayoutState:    json.RawMessage(`{"main_bias":[0.5,0.5]}`),
				Children:       []Node{{ID: "p1", Kind: "pane", PaneID: "p1", Title: "shell"}},
			},
			{
				// The poison: fabricated, so Kitty can never report it back.
				ID: "tab-recovered", Kind: "tab", Title: "Recovered 98b08d66", Layout: "splits",
				Children: []Node{{ID: "p2", Kind: "pane", PaneID: "p2", Title: "shell"}},
			},
		},
	}}
	return StateData{
		SchemaVersion: 5,
		Node:          Host{ID: "origin", Name: "devbox"},
		Workspaces: map[string]*Workspace{
			"ws": {
				ID: "ws", Name: "example-workspace", Revision: 14,
				Panes:    map[string]*Pane{"p1": pane("p1", "shell"), "p2": pane("p2", "shell")},
				Topology: DesiredTopology{Generation: 8, Digest: "stale-full-metadata-digest", Roots: roots},
				Manifest: Manifest{Topology: cloneNodes(roots)},
				Attachments: map[string]*Attachment{
					"local": {
						ID: "local", Node: Host{ID: "origin"}, Endpoint: "unix:/kitty",
						Transport: Transport{Kind: "local"}, Status: AttachmentUnhealthy,
						AppliedTopologyGeneration: 7, AppliedTopologyDigest: "older-digest",
						ReconcileStatus: TopologyReconcileError,
						LastError:       "Kitty topology still differs from generation 8",
						Views:           map[string]RuntimeView{},
					},
				},
				CreatedAt: now, UpdatedAt: now,
			},
		},
		Remotes: map[string]*RemoteCache{},
	}
}

// The upgrade must leave the wedged workspace in a state that converges, and it
// must not need the user to intervene.
func TestWedgedStateRecoversOnUpgrade(t *testing.T) {
	paths := testPaths(testRoot(t))
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(wedgedV5State())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewStore(paths).Load()
	if err != nil {
		t.Fatalf("upgrade refused to load the wedged state: %v", err)
	}
	workspace := loaded.Workspaces["ws"]

	// The digest is recomputed from what is actually stored, so the stale
	// full-metadata digest can no longer make the target unreachable.
	if digest := topologyStructuralDigest(workspace.Topology.Roots); digest != workspace.Topology.Digest {
		t.Fatalf("migrated digest %s does not describe migrated roots (%s)", workspace.Topology.Digest, digest)
	}
	// Structure is preserved: the panes keep their grouping, nothing is lost.
	if !samePaneSet(topologyPaneIDs(workspace.Topology.Roots), map[string]bool{"p1": true, "p2": true}) {
		t.Fatalf("migration lost panes: %#v", topologyPaneIDs(workspace.Topology.Roots))
	}
	for _, pane := range workspace.Panes {
		if !pane.Admitted() {
			t.Fatalf("migration demoted a live pane: %#v", pane)
		}
	}
	// The workspace must be renderable; unrenderable is the unattachable state.
	session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
	if err != nil {
		t.Fatalf("migrated workspace is not renderable: %v", err)
	}
	// And what it renders must come back unchanged from Kitty -- the property
	// the old code violated.
	kitty := &fakeKitty{}
	if err := kitty.LoadSession(session); err != nil {
		t.Fatalf("kitty rejected the migrated session: %v\n%s", err, session)
	}
	observed, err := topologyFromKitty(kitty.LS(), workspace.ID)
	if err != nil {
		t.Fatalf("read back the migrated session: %v", err)
	}
	if observed, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, observed); err != nil {
		t.Fatal(err)
	}
	if !topologyMatchesDesired(workspace, observed) {
		t.Fatalf("migrated topology is still not reproducible\nsession:\n%s", session)
	}
	// The formerly quoted title survives verbatim now.
	for _, tab := range kitty.LS()[0].Tabs {
		if strings.Contains(tab.Title, `"`) {
			t.Fatalf("tab title still acquires literal quotes: %q", tab.Title)
		}
	}
	if backup, err := os.ReadFile(paths.StateFile + ".v5.backup"); err != nil || string(backup) != string(encoded) {
		t.Fatalf("v5 migration backup missing or wrong: %v", err)
	}
}

// The wedged attachment must converge on its first pass after the upgrade, with
// no window destroyed along the way.
func TestWedgedAttachmentConvergesWithoutDestroyingWindows(t *testing.T) {
	paths := testPaths(testRoot(t))
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(wedgedV5State())
	if err := os.WriteFile(paths.StateFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := newTestDaemonAtPaths(t, paths, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := d.getWorkspace("ws")
	if err != nil {
		t.Fatal(err)
	}
	// Kitty shows exactly what the migrated topology describes.
	tree := kittyTreeForTabs(workspace.ID, [][]string{{"p1"}, {"p2"}})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, workspace)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	if err := d.reconcileEndpointTopology(context.Background(), "unix:/kitty"); err != nil {
		t.Fatalf("first reconcile after upgrade failed: %v", err)
	}
	for _, call := range runner.Calls() {
		joined := strings.Join(call.Args, " ")
		for _, destructive := range []string{"goto_session", "close-window"} {
			if strings.Contains(joined, destructive) {
				t.Fatalf("recovery destroyed windows via %q: %#v", destructive, call.Args)
			}
		}
	}
	got, err := d.getWorkspace("ws")
	if err != nil {
		t.Fatal(err)
	}
	attachment := got.Attachments["local"]
	if attachment.Status != AttachmentReady || attachment.ReconcileStatus != TopologyReconcileReady {
		t.Fatalf("attachment did not converge: status=%s reconcile=%s error=%q",
			attachment.Status, attachment.ReconcileStatus, attachment.LastError)
	}
	if attachment.AppliedTopologyGeneration != got.Topology.Generation {
		t.Fatalf("attachment generation %d != workspace generation %d",
			attachment.AppliedTopologyGeneration, got.Topology.Generation)
	}
}
