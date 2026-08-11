package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

type Store struct {
	paths  Paths
	onSave func()
	logger *log.Logger
}

func NewStore(paths Paths) *Store { return &Store{paths: paths} }

// SetLogger installs an optional logger for migration and invariant diagnostics.
func (s *Store) SetLogger(logger *log.Logger) { s.logger = logger }

// SetOnSave installs an optional daemon-only change signal. Standalone store
// users remain unaware of subscriptions, and the callback runs only after a
// durable state replacement succeeds.
func (s *Store) SetOnSave(callback func()) { s.onSave = callback }

func (s *Store) Ensure() error {
	for _, dir := range []string{s.paths.StateDir, s.paths.GeneratedDir, s.paths.AttachmentDir, s.paths.AgentDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure directory %s: %w", dir, err)
		}
	}
	return nil
}

// Load intentionally treats the pre-v3 schemas as empty state. v3 changes
// process ownership: Kitty view closure now removes its zmx backend. Migrating
// the old records would make that ownership ambiguous, so only zka's generated
// files are reset. Schemas v3-v8 migrate to the current schema. Existing zmx
// processes are deliberately left untouched, and v4-v8 receive a one-time
// rollback backup before migration.
func (s *Store) Load() (StateData, error) {
	if err := s.Ensure(); err != nil {
		return StateData{}, err
	}
	b, err := os.ReadFile(s.paths.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return newStateData(), nil
	}
	if err != nil {
		return StateData{}, fmt.Errorf("read state: %w", err)
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &header); err != nil {
		return StateData{}, fmt.Errorf("decode state header: %w", err)
	}
	if header.SchemaVersion == 1 || header.SchemaVersion == 2 {
		if err := os.RemoveAll(filepath.Join(s.paths.StateDir, "snapshots")); err != nil {
			return StateData{}, fmt.Errorf("reset legacy snapshots: %w", err)
		}
		if err := os.RemoveAll(s.paths.GeneratedDir); err != nil {
			return StateData{}, fmt.Errorf("reset legacy generated files: %w", err)
		}
		if err := os.MkdirAll(s.paths.GeneratedDir, 0o700); err != nil {
			return StateData{}, fmt.Errorf("reset generated workspace files: %w", err)
		}
		return newStateData(), nil
	}
	if header.SchemaVersion < 3 || header.SchemaVersion > stateSchemaVersion {
		return StateData{}, fmt.Errorf("unsupported state schema %d (want %d)", header.SchemaVersion, stateSchemaVersion)
	}
	if header.SchemaVersion >= 4 && header.SchemaVersion < stateSchemaVersion {
		if err := s.writeMigrationBackup(b, header.SchemaVersion); err != nil {
			return StateData{}, err
		}
	}
	var state StateData
	if err := json.Unmarshal(b, &state); err != nil {
		return StateData{}, fmt.Errorf("decode state: %w", err)
	}
	if state.Workspaces == nil {
		state.Workspaces = map[string]*Workspace{}
	}
	if state.Remotes == nil {
		state.Remotes = map[string]*RemoteCache{}
	}
	legacy := header.SchemaVersion < 6
	legacyPhases := map[string]map[string]PaneLifecycle{}
	if legacy {
		legacyPhases = decodeLegacyPanePhases(b)
	}
	for _, workspace := range state.Workspaces {
		normalizeWorkspace(workspace)
		if header.SchemaVersion < 9 {
			migrateWorkspaceCredentialBinding(workspace)
		}
		if header.SchemaVersion < 10 {
			migrateWorkspaceAttachmentClaims(workspace)
		}
		if legacy {
			now := time.Now().UTC()
			for id, pane := range workspace.Panes {
				pane.Phase = legacyPhases[workspace.ID][id]
				if pane.Phase == "" {
					pane.Phase = PaneAdmitted
				}
				if pane.PhaseAt.IsZero() {
					pane.PhaseAt = now
				}
			}
		}
		if header.SchemaVersion < 5 {
			if err := applyManifestLaunchOptions(workspace, workspace.Manifest.Session); err != nil {
				s.logf("workspace %s: drop unparsable manifest launch options: %v", workspace.ID, err)
			}
			// Pre-v5 layout_state contains attachment-local Kitty IDs and
			// cannot be made replica-safe without the original runtime tree.
			clearTopologyLayoutState(workspace.Manifest.Topology)
			clearTopologyLayoutState(workspace.Topology.Roots)
		}
		// Snapshot the pre-migration structure so a converged attachment can be
		// carried across the upgrade instead of being forced to rebuild every
		// open Kitty window -- which is exactly the behaviour this release
		// exists to remove.
		converged := map[string]bool{}
		for id, attachment := range workspace.Attachments {
			converged[id] = attachment.AppliedTopologyGeneration == workspace.Topology.Generation &&
				len(attachment.ObservedTopology) != 0 &&
				topologyStructureEqual(attachment.ObservedTopology, workspace.Topology.Roots)
		}
		topologyChanged, degraded := reconcileLoadedTopology(workspace)
		if degraded != nil {
			s.logf("%v", degraded)
		}
		for id, attachment := range workspace.Attachments {
			if legacy && converged[id] && !topologyChanged {
				attachment.AppliedTopologyDigest = workspace.Topology.Digest
				attachment.ObservedTopology = topologyStructuralIdentity(attachment.ObservedTopology)
				continue
			}
			if !legacy && !topologyChanged {
				continue
			}
			attachment.AppliedTopologyGeneration = 0
			attachment.AppliedTopologyDigest = ""
			attachment.ObservedTopology = nil
			attachment.ReconcileTargetGeneration = workspace.Topology.Generation
			attachment.ReconcileStatus = "pending"
		}
	}
	for _, remote := range state.Remotes {
		if remote == nil {
			continue
		}
		if remote.Workspaces == nil {
			remote.Workspaces = map[string]*Workspace{}
		}
		for _, workspace := range remote.Workspaces {
			normalizeWorkspace(workspace)
			if header.SchemaVersion < 9 {
				migrateWorkspaceCredentialBinding(workspace)
			}
			if header.SchemaVersion < 10 {
				migrateWorkspaceAttachmentClaims(workspace)
			}
		}
	}
	// Zero remains meaningful: it denotes a direct-credential backend. Schema
	// migration never terminates a process; recreation is an explicit command.
	state.SchemaVersion = stateSchemaVersion
	return state, nil
}

// v9 claims are all node-owned: OwnerAttachmentID has no post-migration
// writers. There is therefore no safe attachment owner to infer. Clear every
// authoritative and cached claim, advance the revocation boundary, and require
// an explicit attachment-backed re-claim after upgrade.
func migrateWorkspaceAttachmentClaims(workspace *Workspace) {
	if workspace == nil || workspace.CredentialClaim == nil && workspace.PIVBProvider == nil {
		return
	}
	base := workspace.CredentialGeneration
	if workspace.CredentialClaim != nil && workspace.CredentialClaim.Generation > base {
		base = workspace.CredentialClaim.Generation
	}
	if workspace.PIVBProvider != nil && workspace.PIVBProvider.Generation > base {
		base = workspace.PIVBProvider.Generation
	}
	if base != ^uint64(0) {
		workspace.CredentialGeneration = base + 1
	}
	workspace.CredentialClaim = nil
	workspace.PIVBProvider = nil
}

func migrateWorkspaceCredentialBinding(workspace *Workspace) {
	claim := workspace.CredentialClaim
	legacyPIVB := workspace.PIVBProvider
	if claim != nil {
		claim.ProviderSource = "remote"
		claim.OwnerAttachmentID = ""
	}
	if legacyPIVB == nil {
		return
	}
	if claim == nil {
		source := legacyPIVB.Source
		if source == "attachment" {
			source = "remote"
		}
		workspace.CredentialClaim = &CredentialClaim{
			ProviderSource: source, Bundle: legacyPIVB.Bundle, OwnerNodeID: legacyPIVB.OwnerNodeID,
			Generation: legacyPIVB.Generation, State: legacyPIVB.State,
			Capabilities: map[string]CredentialCapabilityStatus{
				credentialCapabilityPIVB: {
					State: legacyPIVB.State, Available: legacyPIVB.State == "ready",
					Detail: legacyPIVB.LastError,
				},
			},
			PIVB: clonePIVBManifest(&legacyPIVB.Manifest), UpdatedAt: legacyPIVB.UpdatedAt,
		}
		workspace.PIVBProvider = nil
		return
	}
	if legacyPIVB.Source == "attachment" && legacyPIVB.OwnerNodeID == claim.OwnerNodeID && legacyPIVB.Bundle == claim.Bundle {
		claim.PIVB = clonePIVBManifest(&legacyPIVB.Manifest)
		if claim.Capabilities == nil {
			claim.Capabilities = map[string]CredentialCapabilityStatus{}
		}
		claim.Capabilities[credentialCapabilityPIVB] = CredentialCapabilityStatus{State: "ready", Available: true}
		workspace.PIVBProvider = nil
		return
	}
	// V8 allowed a remote SSH/OpenPGP owner and an unrelated local PIVB owner.
	// V9 cannot silently choose between them. Preserve the legacy record for
	// diagnostics/recovery and fail the unified binding closed until release or
	// an explicit whole-bundle claim resolves it.
	claim.State = "migration_conflict"
	if claim.Capabilities == nil {
		claim.Capabilities = map[string]CredentialCapabilityStatus{}
	}
	for name, capability := range claim.Capabilities {
		capability.State = "unavailable"
		capability.Available = false
		capability.Detail = appendCredentialDetail(capability.Detail, "legacy split-provider state requires explicit release or transfer")
		claim.Capabilities[name] = capability
	}
	claim.Capabilities[credentialCapabilityPIVB] = CredentialCapabilityStatus{
		State: "unavailable", Available: false, Detail: "legacy local PIVB provider conflicts with the remote bundle owner",
	}
}

// decodeLegacyPanePhases maps the pre-v6 boolean flags onto the explicit pane
// lifecycle. The three flags overlapped and could disagree; removal wins over
// proposal because a pane being torn down is already out of the topology.
func decodeLegacyPanePhases(data []byte) map[string]map[string]PaneLifecycle {
	var legacy struct {
		Workspaces map[string]struct {
			Panes map[string]struct {
				RemovalPending  bool `json:"removal_pending"`
				TopologyPending bool `json:"topology_pending"`
			} `json:"panes"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil
	}
	result := make(map[string]map[string]PaneLifecycle, len(legacy.Workspaces))
	for workspaceID, workspace := range legacy.Workspaces {
		phases := make(map[string]PaneLifecycle, len(workspace.Panes))
		for paneID, pane := range workspace.Panes {
			switch {
			case pane.RemovalPending:
				phases[paneID] = PaneRetiring
			case pane.TopologyPending:
				phases[paneID] = PaneProposed
			default:
				phases[paneID] = PaneAdmitted
			}
		}
		result[workspaceID] = phases
	}
	return result
}

// enforceTopologyInvariants is a last line of defence on the way to disk. The
// digest must always be derivable from the stored tree; the previous code let
// callers rewrite Roots after the digest had been computed, which could persist
// a convergence target that was unreachable by construction. Repairing here
// costs one tree walk against an already O(state) marshal.
func (s *Store) enforceTopologyInvariants(state StateData) {
	for _, workspace := range state.Workspaces {
		if len(workspace.Topology.Roots) == 0 {
			continue
		}
		if digest := topologyStructuralDigest(workspace.Topology.Roots); digest != workspace.Topology.Digest {
			s.logf("workspace %s: repairing topology digest that disagreed with its roots", workspace.ID)
			workspace.Topology.Digest = digest
		}
	}
}

func (s *Store) logf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Printf(format, args...)
}

func (s *Store) writeMigrationBackup(data []byte, version int) error {
	nonce, err := randomID()
	if err != nil {
		return fmt.Errorf("name state migration backup: %w", err)
	}
	path := fmt.Sprintf("%s.v%d.%s.%s.backup", s.paths.StateFile, version, time.Now().UTC().Format("20060102T150405.000000000Z"), nonce)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create state migration backup: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write state migration backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync state migration backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close state migration backup: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	ok = true
	s.logf("wrote schema-v%d state migration backup %s", version, path)
	return nil
}

func clearTopologyLayoutState(nodes []Node) {
	for index := range nodes {
		nodes[index].LayoutState = nil
		clearTopologyLayoutState(nodes[index].Children)
	}
}

func normalizeWorkspace(workspace *Workspace) {
	if workspace.Panes == nil {
		workspace.Panes = map[string]*Pane{}
	}
	if workspace.Attachments == nil {
		workspace.Attachments = map[string]*Attachment{}
	}
	if workspace.CredentialClaim != nil && workspace.CredentialClaim.Capabilities == nil {
		workspace.CredentialClaim.Capabilities = map[string]CredentialCapabilityStatus{}
	}
	if workspace.CredentialClaim != nil && workspace.CredentialClaim.Generation > workspace.CredentialGeneration {
		workspace.CredentialGeneration = workspace.CredentialClaim.Generation
	}
	if workspace.PIVBProvider != nil && workspace.PIVBProvider.Generation > workspace.CredentialGeneration {
		workspace.CredentialGeneration = workspace.PIVBProvider.Generation
	}
	if workspace.CredentialClaim != nil && workspace.CredentialClaim.ProviderSource == "" {
		if workspace.CredentialClaim.OwnerAttachmentID == "" && workspace.CredentialClaim.OwnerNodeID == workspace.Origin.ID {
			workspace.CredentialClaim.ProviderSource = "local"
		} else {
			workspace.CredentialClaim.ProviderSource = "remote"
		}
	}
	for _, pane := range workspace.Panes {
		if pane.Notifications == nil {
			pane.Notifications = map[string]NotificationRecord{}
		}
	}
	for _, attachment := range workspace.Attachments {
		if attachment.Views == nil {
			attachment.Views = map[string]RuntimeView{}
		}
		if attachment.ClientHeartbeats == nil {
			attachment.ClientHeartbeats = map[string]time.Time{}
		}
	}
	if len(workspace.Topology.Roots) != 0 {
		workspace.Topology.Roots = canonicalTopology(workspace.Topology.Roots)
		if workspace.Topology.Digest == "" {
			workspace.Topology.Digest = topologyDigest(workspace.Topology.Roots)
		}
		if workspace.Topology.Generation == 0 {
			workspace.Topology.Generation = 1
		}
	}
	workspace.RecomputeAttention()
}

func (s *Store) Save(state StateData) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	s.enforceTopologyInvariants(state)
	state.SchemaVersion = stateSchemaVersion
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')
	if err := atomicWrite(s.paths.StateFile, b, 0o600); err != nil {
		return err
	}
	if s.onSave != nil {
		s.onSave()
	}
	return nil
}

func (s *Store) SessionPath(workspaceID, suffix string) string {
	name := shortID(workspaceID)
	if suffix != "" {
		name += "-" + suffix
	}
	return filepath.Join(s.paths.GeneratedDir, name+".kitty-session")
}

func (s *Store) WriteSession(workspaceID, suffix, content string) (string, error) {
	if err := s.Ensure(); err != nil {
		return "", err
	}
	path := s.SessionPath(workspaceID, suffix)
	if err := atomicWrite(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) RemoveWorkspaceSessions(workspaceID string) error {
	pattern := filepath.Join(s.paths.GeneratedDir, shortID(workspaceID)+"*.kitty-session")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("find generated workspace sessions: %w", err)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove generated workspace session %s: %w", path, err)
		}
	}
	return nil
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".zka-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ok = true
	return nil
}
