package zka

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	credentialStreamHelloMax = 8 << 10
	// The manifest travels inside a remote control envelope whose total limit is
	// one MiB. Leave ample room for JSON escaping and the surrounding request.
	openPGPPublicManifestMax = 512 << 10
)

const (
	credentialCapabilitySSH     = "ssh-agent"
	credentialCapabilityOpenPGP = "openpgp"
	credentialCapabilityPIVB    = "pivb"
)

type credentialBundleManifest struct {
	Bundle  string                     `json:"bundle"`
	SSH     bool                       `json:"ssh_agent"`
	OpenPGP *credentialOpenPGPManifest `json:"openpgp,omitempty"`
	PIVB    *CredentialPIVBManifest    `json:"pivb,omitempty"`
}

type credentialOpenPGPManifest struct {
	Fingerprints    []string          `json:"fingerprints"`
	PublicKeys      string            `json:"public_keys"`
	AllowedKeygrips map[string]string `json:"allowed_keygrips"`
	CardBacked      map[string]bool   `json:"card_backed,omitempty"`
}

type workspaceCredentialRequest struct {
	Workspace         string                   `json:"workspace"`
	Bundle            string                   `json:"bundle,omitempty"`
	IfUnclaimed       bool                     `json:"if_unclaimed,omitempty"`
	Provider          Host                     `json:"provider,omitempty"`
	ProviderSource    string                   `json:"provider_source,omitempty"`
	CallerSSHAuthSock string                   `json:"caller_ssh_auth_sock,omitempty"`
	Manifest          credentialBundleManifest `json:"manifest,omitempty"`
}

type credentialStreamHello struct {
	Workspace  string `json:"workspace"`
	Bundle     string `json:"bundle"`
	Capability string `json:"capability"`
	Generation uint64 `json:"generation"`
}

type credentialCapabilityView struct {
	State     string `json:"state"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

type workspaceCredentialStatus struct {
	WorkspaceID      string                              `json:"workspace_id"`
	WorkspaceName    string                              `json:"workspace_name"`
	Bundle           string                              `json:"bundle,omitempty"`
	OwnerNode        string                              `json:"owner_node,omitempty"`
	Generation       uint64                              `json:"generation,omitempty"`
	State            string                              `json:"state"`
	Capabilities     map[string]credentialCapabilityView `json:"capabilities"`
	RecreatePaneIDs  []string                            `json:"panes_requiring_recreation,omitempty"`
	RecreationDetail string                              `json:"pane_recreation_detail,omitempty"`
	// providerSelected is process-local control flow, not API state. It tells
	// activate-local whether this call selected/refreshed the requested local
	// provider or returned an untouched --if-unclaimed binding.
	providerSelected bool
}

type credentialStatusResponse struct {
	Transport        credentialTransportView     `json:"transport"`
	Workspaces       []workspaceCredentialStatus `json:"workspaces"`
	ActiveOperations []credentialActiveOperation `json:"active_operations"`
}

type credentialTransportView struct {
	State       string    `json:"state"`
	Attempts    int       `json:"attempts,omitempty"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type credentialTransportSessionRequest struct {
	Provider Host   `json:"provider"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint,omitempty"`
}

type incomingCredentialTransport struct {
	Provider  Host
	State     string
	Endpoint  string
	UpdatedAt time.Time
}

type credentialActiveOperation struct {
	WorkspaceID string `json:"workspace_id"`
	Capability  string `json:"capability"`
	Operation   string `json:"operation"`
	Count       int    `json:"count"`
}

func buildCredentialBundleManifest(ctx context.Context, cfg Config, bundleName string, runner CommandRunner) (credentialBundleManifest, error) {
	return buildCredentialBundleManifestForSocket(ctx, cfg, bundleName, runner, os.Getenv("SSH_AUTH_SOCK"))
}

func buildCredentialBundleManifestForSocket(ctx context.Context, cfg Config, bundleName string, runner CommandRunner, inheritedSSHSocket string) (credentialBundleManifest, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	bundle, ok := cfg.credentialBundle(bundleName)
	if !ok {
		return credentialBundleManifest{}, fmt.Errorf("unknown credential bundle %q", bundleName)
	}
	manifest := credentialBundleManifest{Bundle: bundleName, SSH: bundle.SSHAgent.Enable}
	if bundle.SSHAgent.Enable {
		info := newSSHAgentInfo(cfg, inheritedSSHSocket)
		conn, err := dialAgentSocket(info.EffectiveSocket)
		if err != nil {
			return credentialBundleManifest{}, fmt.Errorf("credential bundle %q SSH agent: %w", bundleName, err)
		}
		_ = conn.Close()
	}
	if bundle.OpenPGP.Enable {
		openpgp, err := buildOpenPGPManifest(ctx, cfg, bundle.OpenPGP.SigningKeys, runner)
		if err != nil {
			return credentialBundleManifest{}, fmt.Errorf("credential bundle %q OpenPGP: %w", bundleName, err)
		}
		manifest.OpenPGP = openpgp
	}
	if bundle.PIVB.Enable {
		pivb, err := buildPIVBManifest(ctx, cfg, bundle.PIVB.Aliases)
		if err != nil {
			return credentialBundleManifest{}, fmt.Errorf("credential bundle %q PIVB: %w", bundleName, err)
		}
		manifest.PIVB = pivb
	}
	return manifest, nil
}

func buildOpenPGPManifest(ctx context.Context, cfg Config, fingerprints []string, runner CommandRunner) (*credentialOpenPGPManifest, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	if len(fingerprints) == 0 {
		return nil, fmt.Errorf("no OpenPGP provider signing fingerprints are configured")
	}
	args := []string{"--batch", "--with-colons", "--with-keygrip", "--list-secret-keys"}
	args = append(args, fingerprints...)
	listing, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.Command, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve signing keygrips: %w", err)
	}
	keygrips, found, err := parseOpenPGPKeygrips(listing, fingerprints)
	if err != nil {
		return nil, err
	}
	for _, fingerprint := range fingerprints {
		if !found[fingerprint] {
			return nil, fmt.Errorf("configured signing fingerprint %s was not found in the provider secret keyring", fingerprint)
		}
	}
	if len(keygrips) == 0 {
		return nil, fmt.Errorf("configured fingerprints resolve to no signing or decryption keygrips")
	}
	exportArgs := []string{"--batch", "--armor", "--export-options", "export-minimal", "--export"}
	exportArgs = append(exportArgs, fingerprints...)
	publicKeys, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.Command, exportArgs...)
	if err != nil {
		return nil, fmt.Errorf("export public keys: %w", err)
	}
	if publicKeys == "" {
		return nil, fmt.Errorf("public-key export was empty")
	}
	if len(publicKeys) > openPGPPublicManifestMax {
		return nil, fmt.Errorf("public-key export exceeds %d bytes", openPGPPublicManifestMax)
	}
	extraSocket, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.GPGConfCommand, "--list-dirs", "agent-extra-socket")
	if err != nil {
		return nil, fmt.Errorf("locate restricted agent extra socket: %w", err)
	}
	conn, err := net.DialTimeout("unix", strings.TrimSpace(extraSocket), 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("restricted agent extra socket is unavailable: %w", err)
	}
	_ = conn.Close()
	cardBacked := map[string]bool{}
	for grip := range keygrips {
		cardBacked[grip] = openPGPKeygripCardBacked(strings.TrimSpace(extraSocket), grip)
	}
	return &credentialOpenPGPManifest{
		Fingerprints: append([]string(nil), fingerprints...), PublicKeys: publicKeys,
		AllowedKeygrips: keygrips, CardBacked: cardBacked,
	}, nil
}

func parseOpenPGPKeygrips(listing string, configured []string) (map[string]string, map[string]bool, error) {
	wanted := map[string]bool{}
	for _, fingerprint := range configured {
		wanted[fingerprint] = true
	}
	allowed := map[string]string{}
	found := map[string]bool{}
	primaryFingerprint := ""
	recordType := ""
	recordCapabilities := ""
	recordFingerprint := ""
	scanner := bufio.NewScanner(strings.NewReader(listing))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "sec", "ssb":
			recordType = fields[0]
			recordCapabilities = ""
			if len(fields) > 11 {
				recordCapabilities = strings.ToLower(fields[11])
			}
			recordFingerprint = ""
		case "fpr":
			if recordType == "" || len(fields) <= 9 {
				continue
			}
			fingerprint, err := canonicalOpenPGPFingerprint(fields[9])
			if err != nil {
				return nil, nil, fmt.Errorf("invalid fingerprint in gpg listing: %w", err)
			}
			recordFingerprint = fingerprint
			if recordType == "sec" {
				primaryFingerprint = fingerprint
			}
			if wanted[fingerprint] {
				found[fingerprint] = true
			}
		case "grp":
			if recordFingerprint == "" || len(fields) <= 9 ||
				(!strings.Contains(recordCapabilities, "s") && !strings.Contains(recordCapabilities, "e")) {
				continue
			}
			grip := strings.ToUpper(fields[9])
			if len(grip) != 40 {
				return nil, nil, fmt.Errorf("invalid keygrip %q in gpg listing", grip)
			}
			if wanted[recordFingerprint] || wanted[primaryFingerprint] {
				allowed[grip] = recordFingerprint
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return allowed, found, nil
}

func credentialOpenPGPHome(paths Paths, workspaceID string) (string, error) {
	if workspaceID == "" || filepath.Base(workspaceID) != workspaceID {
		return "", fmt.Errorf("invalid workspace id for OpenPGP home")
	}
	return filepath.Join(paths.StateDir, "credentials", workspaceID, "gnupg"), nil
}

func ensureGPGSocketDirectory(ctx context.Context, gpgconfCommand, home string, runner CommandRunner) error {
	if runner == nil {
		runner = ExecRunner{}
	}
	socketDir, _, err := runner.Run(ctx, gpgconfCommand, "--homedir", home, "--list-dirs", "socketdir")
	if err != nil {
		return fmt.Errorf("locate GnuPG socket directory: %w", err)
	}
	socketDir = strings.TrimSpace(socketDir)
	if socketDir == "" || !filepath.IsAbs(socketDir) {
		return fmt.Errorf("gpgconf returned invalid socket directory %q", socketDir)
	}
	info, statErr := os.Stat(socketDir)
	if statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("GnuPG socket path %s is not a directory", socketDir)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("GnuPG socket directory %s has insecure mode %04o", socketDir, info.Mode().Perm())
		}
		return nil
	}
	if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect GnuPG socket directory: %w", statErr)
	}
	if _, _, err := runner.Run(ctx, gpgconfCommand, "--homedir", home, "--create-socketdir"); err != nil {
		return fmt.Errorf("create GnuPG socket directory: %w", err)
	}
	info, err = os.Stat(socketDir)
	if err != nil {
		return fmt.Errorf("verify GnuPG socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("GnuPG socket directory %s is not a private directory", socketDir)
	}
	return nil
}

func prepareOpenPGPTarget(ctx context.Context, paths Paths, cfg Config, workspaceID string, manifest *credentialOpenPGPManifest, runner CommandRunner, logger *log.Logger) (string, error) {
	if manifest == nil || len(manifest.Fingerprints) == 0 || manifest.PublicKeys == "" {
		return "", fmt.Errorf("OpenPGP manifest is incomplete")
	}
	if len(manifest.PublicKeys) > openPGPPublicManifestMax {
		return "", fmt.Errorf("OpenPGP public-key manifest exceeds %d bytes", openPGPPublicManifestMax)
	}
	home, err := credentialOpenPGPHome(paths, workspaceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create OpenPGP home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return "", fmt.Errorf("secure OpenPGP home: %w", err)
	}
	for _, name := range []string{"common.conf", "gpg.conf"} {
		if err := atomicWrite(filepath.Join(home, name), []byte("no-autostart\n"), 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	publicPath := filepath.Join(home, "zka-public.asc")
	if err := atomicWrite(publicPath, []byte(manifest.PublicKeys), 0o600); err != nil {
		return "", fmt.Errorf("write public-key manifest: %w", err)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if err := ensureGPGSocketDirectory(ctx, cfg.Credentials.GnuPG.GPGConfCommand, home, runner); err != nil {
		return "", err
	}
	// GnuPG can import the public keys and then exit non-zero after probing the
	// stable workspace relay. The fingerprint listing below is authoritative.
	importStdout, importStderr, importErr := runner.Run(ctx, cfg.Credentials.GnuPG.Command, "--homedir", home, "--batch", "--yes", "--import", publicPath)
	listArgs := []string{"--homedir", home, "--batch", "--with-colons", "--fingerprint", "--list-keys"}
	listArgs = append(listArgs, manifest.Fingerprints...)
	listing, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.Command, listArgs...)
	if err != nil {
		return "", fmt.Errorf("validate imported public keys: %w", err)
	}
	found := map[string]bool{}
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			fingerprint, fingerprintErr := canonicalOpenPGPFingerprint(fields[9])
			if fingerprintErr == nil {
				found[fingerprint] = true
			}
		}
	}
	for _, rawFingerprint := range manifest.Fingerprints {
		fingerprint, fingerprintErr := canonicalOpenPGPFingerprint(rawFingerprint)
		if fingerprintErr != nil || !found[fingerprint] {
			detail := openPGPImportDiagnostic(importStdout, importStderr, importErr)
			if detail != "" {
				return "", fmt.Errorf("public-key manifest does not contain configured fingerprint %s; gpg import: %s", rawFingerprint, detail)
			}
			return "", fmt.Errorf("public-key manifest does not contain configured fingerprint %s", rawFingerprint)
		}
	}
	if importErr != nil && logger != nil {
		detail := strings.Join(strings.Fields(strings.TrimSpace(importStderr)), " ")
		label := "stderr"
		if detail == "" {
			detail = strings.Join(strings.Fields(importErr.Error()), " ")
			label = "error"
		}
		logger.Printf("OpenPGP import advisory (expected): gpg --import exited non-zero for workspace %s; fingerprint validation passed; %s=%q", workspaceID, label, detail)
	}
	socket, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.GPGConfCommand, "--homedir", home, "--list-dirs", "agent-socket")
	if err != nil {
		return "", fmt.Errorf("locate target GnuPG agent socket: %w", err)
	}
	socket = strings.TrimSpace(socket)
	if socket == "" || !filepath.IsAbs(socket) {
		return "", fmt.Errorf("gpgconf returned invalid target agent socket %q", socket)
	}
	return socket, nil
}

func openPGPImportDiagnostic(stdout, stderr string, err error) string {
	parts := make([]string, 0, 3)
	if detail := strings.TrimSpace(stdout); detail != "" {
		parts = append(parts, fmt.Sprintf("stdout=%q", detail))
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		parts = append(parts, fmt.Sprintf("stderr=%q", detail))
	}
	if err != nil {
		parts = append(parts, fmt.Sprintf("error=%q", err.Error()))
	}
	return strings.Join(parts, ", ")
}

// Credential claim state machine:
//
//	                     ┌── failure ──► prior claim (or unclaimed)
//	                     │
//	unclaimed ── claim ──► preparing ── success ──► claimed
//	                                                 │
//	                       explicit release ◄─────────┤
//	                                                 └─ transport loss ─► claimed/degraded
//
// Preparation is deliberately performed before the durable swap. A failed
// capability therefore cannot publish a partial claim, and reconnect keeps the
// durable owner while listeners fail closed until they are reconciled.
func (d *Daemon) claimWorkspaceCredentials(ctx context.Context, req workspaceCredentialRequest) (workspaceCredentialStatus, error) {
	if req.Workspace == "" || req.Bundle == "" {
		return workspaceCredentialStatus{}, fmt.Errorf("workspace and bundle are required")
	}
	bundle, ok := d.config.credentialBundle(req.Bundle)
	if !ok {
		return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q is not configured on this node", req.Bundle)
	}
	if req.Manifest.Bundle != req.Bundle || req.Manifest.SSH != bundle.SSHAgent.Enable ||
		(req.Manifest.OpenPGP != nil) != bundle.OpenPGP.Enable || (req.Manifest.PIVB != nil) != bundle.PIVB.Enable {
		return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q capability manifest does not match the target configuration", req.Bundle)
	}
	if bundle.PIVB.Enable && !samePIVBPolicy(bundle, req.Manifest.PIVB) {
		return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q PIVB manifest does not match its alias policy or card identity", req.Bundle)
	}
	if bundle.PIVB.Enable {
		if err := validatePIVBClaimAgainstLocalPolicy(ctx, d.config, req.Manifest.PIVB); err != nil {
			return workspaceCredentialStatus{}, fmt.Errorf("credential bundle %q PIVB policy: %w", req.Bundle, err)
		}
	}
	d.mu.Lock()
	initial, initialErr := d.resolveWorkspaceLocked(req.Workspace)
	d.mu.Unlock()
	if initialErr != nil {
		return workspaceCredentialStatus{}, initialErr
	}
	claimLock := d.credentialClaimLock(initial.ID)
	claimLock.Lock()
	defer claimLock.Unlock()

	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(req.Workspace)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	if err := requireWorkspaceMutable(workspace); err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if req.IfUnclaimed && workspace.CredentialClaim != nil {
		workspaceID := workspace.ID
		d.mu.Unlock()
		return d.workspaceCredentialStatus(workspaceID)
	}
	provider := req.Provider
	source := req.ProviderSource
	if source == "" {
		source = "remote"
	}
	if provider.ID == "" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("credential provider node is required")
	}
	if source != "local" && source != "remote" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("invalid credential provider source %q", source)
	}
	if source == "local" && provider.ID != d.state.Node.ID {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("local credential provider must be this workspace origin")
	}
	if workspace.CredentialClaim != nil && len(panesRequiringCredentialEnvironment(workspace)) != 0 {
		existing := workspace.CredentialClaim
		if existing.ProviderSource != source || existing.OwnerNodeID != provider.ID || existing.Bundle != req.Bundle {
			d.mu.Unlock()
			return workspaceCredentialStatus{}, fmt.Errorf("credential provider transfer is blocked until version 0 panes complete managed-environment migration")
		}
	}
	workspaceID := workspace.ID
	ownerNode := provider.ID
	previousGeneration := uint64(0)
	if workspace.CredentialClaim != nil {
		previousGeneration = workspace.CredentialClaim.Generation
	}
	d.mu.Unlock()
	if source == "remote" && !d.incomingCredentialTransportReady(ownerNode) {
		return workspaceCredentialStatus{}, fmt.Errorf("credential provider node %s has no ready control transport", ownerNode)
	}

	capabilities := map[string]CredentialCapabilityStatus{}
	if bundle.SSHAgent.Enable {
		capabilities[credentialCapabilitySSH] = CredentialCapabilityStatus{State: "ready", Available: true}
	}
	if bundle.OpenPGP.Enable {
		if _, err := prepareOpenPGPTarget(ctx, d.paths, d.config, workspaceID, req.Manifest.OpenPGP, d.runner, d.logger); err != nil {
			return workspaceCredentialStatus{}, err
		}
		detail := ""
		for grip, cardBacked := range req.Manifest.OpenPGP.CardBacked {
			if !cardBacked {
				detail = "warning: signing keygrip " + grip + " is not card-backed"
				break
			}
		}
		capabilities[credentialCapabilityOpenPGP] = CredentialCapabilityStatus{State: "ready", Available: true, Detail: detail}
	}
	if bundle.PIVB.Enable {
		capabilities[credentialCapabilityPIVB] = CredentialCapabilityStatus{
			State: "ready", Available: true,
			Detail: fmt.Sprintf("YubiKey %d key %s", req.Manifest.PIVB.Card.Serial, req.Manifest.PIVB.Card.KeyID),
		}
	}
	// Stable pane-facing listeners are part of claim preparation. Publishing a
	// durable ready claim before those paths can be bound would make creator
	// defaults appear successful while every client fails at connect time.
	d.reconcileCredentialRoutes(ctx)
	if err := d.validateCredentialRoutes(workspaceID, capabilities); err != nil {
		return workspaceCredentialStatus{}, err
	}

	d.mu.Lock()
	workspace, err = d.resolveWorkspaceLocked(workspaceID)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if source == "remote" && !d.incomingCredentialTransportReady(ownerNode) {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("credential provider transport changed while preparing credential claim")
	}
	keys := []string(nil)
	if req.Manifest.OpenPGP != nil {
		keys = append(keys, req.Manifest.OpenPGP.Fingerprints...)
	}
	previousClaim := workspace.CredentialClaim
	previousUpdatedAt := workspace.UpdatedAt
	if credentialClaimMatchesManifest(previousClaim, source, ownerNode, req.Bundle, req.Manifest) {
		d.mu.Unlock()
		d.reconcileCredentialRoutes(ctx)
		status, statusErr := d.workspaceCredentialStatus(workspaceID)
		status.providerSelected = statusErr == nil
		return status, statusErr
	}
	previousCredentialGeneration := workspace.CredentialGeneration
	generationBase := previousCredentialGeneration
	if previousClaim != nil && previousClaim.Generation > generationBase {
		generationBase = previousClaim.Generation
	}
	if generationBase == ^uint64(0) {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("credential generation exhausted for workspace %s", workspaceID)
	}
	newGeneration := generationBase + 1
	workspace.CredentialGeneration = newGeneration
	workspace.CredentialClaim = &CredentialClaim{
		ProviderSource: source, Bundle: req.Bundle, OwnerNodeID: ownerNode,
		Generation: newGeneration, State: "ready", Capabilities: capabilities,
		OpenPGPKeys: keys, PIVB: clonePIVBManifest(req.Manifest.PIVB), UpdatedAt: time.Now().UTC(),
	}
	workspace.PIVBProvider = nil
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		workspace.CredentialClaim = previousClaim
		workspace.CredentialGeneration = previousCredentialGeneration
		workspace.UpdatedAt = previousUpdatedAt
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	d.mu.Unlock()
	d.revokeCredentialRoutes(workspaceID, previousGeneration)
	d.reconcileCredentialRoutes(ctx)
	status, statusErr := d.workspaceCredentialStatus(workspaceID)
	status.providerSelected = statusErr == nil
	return status, statusErr
}

func credentialClaimMatchesManifest(claim *CredentialClaim, source, ownerNode, bundle string, manifest credentialBundleManifest) bool {
	if claim == nil || claim.ProviderSource != source || claim.OwnerNodeID != ownerNode || claim.Bundle != bundle || claim.State != "ready" ||
		!sameStringSet(claim.OpenPGPKeys, manifestOpenPGPFingerprints(manifest.OpenPGP)) {
		return false
	}
	left, _ := json.Marshal(claim.PIVB)
	right, _ := json.Marshal(manifest.PIVB)
	return bytes.Equal(left, right)
}

func manifestOpenPGPFingerprints(manifest *credentialOpenPGPManifest) []string {
	if manifest == nil {
		return nil
	}
	return manifest.Fingerprints
}

func (d *Daemon) releaseWorkspaceCredentials(workspaceRef string) (workspaceCredentialStatus, error) {
	d.mu.Lock()
	initial, initialErr := d.resolveWorkspaceLocked(workspaceRef)
	d.mu.Unlock()
	if initialErr != nil {
		return workspaceCredentialStatus{}, initialErr
	}
	claimLock := d.credentialClaimLock(initial.ID)
	claimLock.Lock()
	defer claimLock.Unlock()
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	if workspace.RemoteHost != "" {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, fmt.Errorf("workspace %q is not authoritative on this host", workspace.Name)
	}
	previousClaim := workspace.CredentialClaim
	previousUpdatedAt := workspace.UpdatedAt
	previousGeneration := uint64(0)
	if previousClaim != nil {
		previousGeneration = previousClaim.Generation
	}
	workspace.CredentialClaim = nil
	workspace.PIVBProvider = nil
	workspace.UpdatedAt = time.Now().UTC()
	if err := d.store.Save(d.state); err != nil {
		workspace.CredentialClaim = previousClaim
		workspace.UpdatedAt = previousUpdatedAt
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	workspaceID := workspace.ID
	d.mu.Unlock()
	d.revokeCredentialRoutes(workspaceID, previousGeneration)
	return d.workspaceCredentialStatus(workspaceID)
}

func (d *Daemon) credentialClaimLock(workspaceID string) *sync.Mutex {
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()
	lock := d.credentialClaims[workspaceID]
	if lock == nil {
		lock = &sync.Mutex{}
		d.credentialClaims[workspaceID] = lock
	}
	return lock
}

func (d *Daemon) workspaceCredentialStatus(workspaceRef string) (workspaceCredentialStatus, error) {
	d.mu.Lock()
	workspace, err := d.resolveWorkspaceLocked(workspaceRef)
	if err != nil {
		d.mu.Unlock()
		return workspaceCredentialStatus{}, err
	}
	status := credentialStatusFromWorkspace(workspace)
	if workspace.CredentialClaim == nil {
		d.mu.Unlock()
		return status, nil
	}
	workspaceSnapshot := workspace.Clone()
	d.mu.Unlock()
	if !d.credentialClaimTransportReady(workspaceSnapshot) {
		degradeCredentialStatus(&status, workspaceSnapshot)
	}
	d.degradeMissingCredentialSSHSource(&status, workspaceSnapshot)
	d.degradeMissingCredentialRoutes(&status, workspaceSnapshot)
	return status, nil
}

func sortedCredentialStatuses(workspaces []*Workspace) []workspaceCredentialStatus {
	statuses := make([]workspaceCredentialStatus, 0, len(workspaces))
	for _, workspace := range workspaces {
		statuses = append(statuses, credentialStatusFromWorkspace(workspace))
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].WorkspaceName != statuses[j].WorkspaceName {
			return statuses[i].WorkspaceName < statuses[j].WorkspaceName
		}
		return statuses[i].WorkspaceID < statuses[j].WorkspaceID
	})
	return statuses
}

func credentialStatusFromWorkspace(workspace *Workspace) workspaceCredentialStatus {
	status := workspaceCredentialStatus{
		WorkspaceID: workspace.ID, WorkspaceName: workspace.Name, State: "unclaimed",
		Capabilities: map[string]credentialCapabilityView{},
	}
	claim := workspace.CredentialClaim
	if claim == nil {
		status.RecreatePaneIDs = panesRequiringCredentialEnvironment(workspace)
		if len(status.RecreatePaneIDs) != 0 {
			status.RecreationDetail = "version 0 panes are awaiting automatic managed-credential migration"
			status.RecreationDetail = appendCredentialMigrationErrors(status.RecreationDetail, workspace, status.RecreatePaneIDs)
		}
		return status
	}

	status.Bundle, status.OwnerNode = claim.Bundle, claim.OwnerNodeID
	status.Generation, status.State = claim.Generation, claim.State
	for name, capability := range claim.Capabilities {
		status.Capabilities[name] = credentialCapabilityView{State: capability.State, Available: capability.Available, Detail: capability.Detail}
	}
	status.RecreatePaneIDs = panesRequiringCredentialEnvironment(workspace)
	if len(status.RecreatePaneIDs) == 0 {
		return status
	}
	status.RecreationDetail = "version 0 panes are awaiting automatic managed-credential migration; provider transfer is blocked until migration completes"
	status.RecreationDetail = appendCredentialMigrationErrors(status.RecreationDetail, workspace, status.RecreatePaneIDs)
	if _, ok := claim.Capabilities[credentialCapabilitySSH]; ok {
		capability := status.Capabilities[credentialCapabilitySSH]
		capability.Detail = appendCredentialDetail(capability.Detail, "some panes require recreation to receive SSH_AUTH_SOCK")
		status.Capabilities[credentialCapabilitySSH] = capability
	}
	if _, ok := claim.Capabilities[credentialCapabilityOpenPGP]; ok {
		capability := status.Capabilities[credentialCapabilityOpenPGP]
		capability.Detail = appendCredentialDetail(capability.Detail, "some panes require recreation to receive GNUPGHOME")
		status.Capabilities[credentialCapabilityOpenPGP] = capability
	}
	return status
}

func appendCredentialMigrationErrors(detail string, workspace *Workspace, paneIDs []string) string {
	var failures []string
	for _, paneID := range paneIDs {
		if pane := workspace.Panes[paneID]; pane != nil && pane.CredentialMigrationError != "" {
			failures = append(failures, paneID+": "+pane.CredentialMigrationError)
		}
	}
	if len(failures) == 0 {
		return detail
	}
	return appendCredentialDetail(detail, strings.Join(failures, "; "))
}

func appendCredentialDetail(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func writeCredentialStreamHello(conn net.Conn, hello credentialStreamHello) error {
	raw, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	if len(raw)+1 > credentialStreamHelloMax {
		return fmt.Errorf("credential stream hello exceeds %d bytes", credentialStreamHelloMax)
	}
	raw = append(raw, '\n')
	_, err = conn.Write(raw)
	return err
}

func readCredentialStreamHello(conn net.Conn) (credentialStreamHello, error) {
	line := make([]byte, 0, 256)
	var one [1]byte
	for len(line) <= credentialStreamHelloMax {
		n, err := conn.Read(one[:])
		if err != nil {
			return credentialStreamHello{}, err
		}
		if n == 1 {
			line = append(line, one[0])
			if one[0] == '\n' {
				break
			}
		}
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return credentialStreamHello{}, fmt.Errorf("credential stream hello exceeds %d bytes", credentialStreamHelloMax)
	}
	var hello credentialStreamHello
	if err := json.Unmarshal(line, &hello); err != nil {
		return credentialStreamHello{}, fmt.Errorf("decode credential stream hello: %w", err)
	}
	return hello, nil
}

func (d *Daemon) handleCredentialStream(ctx context.Context, host string, stream net.Conn) {
	hello, err := readCredentialStreamHello(stream)
	if err != nil {
		return
	}
	claim, err := d.remoteCredentialClaim(ctx, host, hello)
	if err != nil {
		return
	}
	bundle, ok := d.config.credentialBundle(claim.Bundle)
	if !ok {
		return
	}
	var upstream net.Conn
	switch hello.Capability {
	case credentialCapabilitySSH:
		if !bundle.SSHAgent.Enable {
			return
		}
		d.credentialMu.Lock()
		socket := d.credentialSSHSources[credentialSSHSourceKey(host, hello.Workspace, hello.Generation)]
		d.credentialMu.Unlock()
		if socket == "" && d.sshAgent.IdentityAgent != "" {
			socket = d.sshAgent.EffectiveSocket
		}
		if socket == "" {
			d.logger.Printf("credential SSH source unavailable host=%s workspace=%s generation=%d: provider restarted without ssh.identity_agent; re-claim credentials", host, hello.Workspace, hello.Generation)
			return
		}
		upstream, err = dialAgentSocket(socket)
	case credentialCapabilityOpenPGP:
		if !bundle.OpenPGP.Enable {
			return
		}
		manifest, manifestErr := d.openPGPManifestForClaim(ctx, host, hello, bundle)
		if manifestErr != nil {
			return
		}
		_ = d.filterOpenPGPStream(ctx, host, hello, manifest, stream)
		return
	case credentialCapabilityPIVB:
		if !bundle.PIVB.Enable || claim.PIVB == nil || !samePIVBPolicy(bundle, claim.PIVB) {
			return
		}
		d.mu.Lock()
		originNode := ""
		if remote := d.state.Remotes[host]; remote != nil {
			if workspace := remote.Workspaces[hello.Workspace]; workspace != nil {
				originNode = workspace.Origin.ID
			}
		}
		d.mu.Unlock()
		if originNode == "" {
			return
		}
		_ = d.proxyPIVBMint(ctx, stream, hello.Workspace, hello.Bundle, hello.Generation, "", originNode, claim.PIVB)
		return
	default:
		return
	}
	if err != nil {
		return
	}
	defer upstream.Close()
	proxyCredentialConnections(ctx, stream, upstream)
}

func credentialSSHSourceKey(host, workspace string, generation uint64) string {
	return host + "\x00" + workspace + "\x00" + fmt.Sprintf("%d", generation)
}

func (d *Daemon) setCredentialSSHSource(host, workspace string, generation uint64, socket string) {
	if workspace == "" || generation == 0 || socket == "" || socket == "none" {
		return
	}
	d.credentialMu.Lock()
	d.credentialSSHSources[credentialSSHSourceKey(host, workspace, generation)] = socket
	d.credentialMu.Unlock()
}

func cloneCredentialOpenPGPManifest(manifest *credentialOpenPGPManifest) *credentialOpenPGPManifest {
	if manifest == nil {
		return nil
	}
	raw, _ := json.Marshal(manifest)
	var clone credentialOpenPGPManifest
	_ = json.Unmarshal(raw, &clone)
	return &clone
}

func (d *Daemon) credentialSSHSocketForCaller(callerSocket string) string {
	if d.sshAgent.IdentityAgent != "" || callerSocket == "" {
		return d.sshAgent.EffectiveSocket
	}
	return callerSocket
}

func (d *Daemon) clearCredentialProviderSources(host, workspace string) {
	prefix := host + "\x00" + workspace + "\x00"
	d.credentialMu.Lock()
	for key := range d.credentialSSHSources {
		if strings.HasPrefix(key, prefix) {
			delete(d.credentialSSHSources, key)
		}
	}
	for key := range d.credentialOpenPGP {
		if strings.HasPrefix(key, prefix) {
			delete(d.credentialOpenPGP, key)
		}
	}
	d.credentialMu.Unlock()
}

func (d *Daemon) reconcileCredentialProviderSources(req remoteDaemonRequest, result json.RawMessage) {
	var request workspaceCredentialRequest
	if json.Unmarshal(req.Payload, &request) != nil || request.Workspace == "" {
		return
	}
	switch req.Op {
	case "credentials_claim":
		var status workspaceCredentialStatus
		if json.Unmarshal(result, &status) != nil || status.Generation == 0 {
			return
		}
		d.clearCredentialProviderSources(req.Host, request.Workspace)
		key := credentialSSHSourceKey(req.Host, request.Workspace, status.Generation)
		if _, enabled := status.Capabilities[credentialCapabilitySSH]; enabled {
			d.setCredentialSSHSource(req.Host, request.Workspace, status.Generation, d.credentialSSHSocketForCaller(req.CallerSSHAuthSock))
		}
		if _, enabled := status.Capabilities[credentialCapabilityOpenPGP]; enabled && request.Manifest.OpenPGP != nil {
			d.credentialMu.Lock()
			if d.credentialOpenPGP == nil {
				d.credentialOpenPGP = map[string]*credentialOpenPGPManifest{}
			}
			d.credentialOpenPGP[key] = cloneCredentialOpenPGPManifest(request.Manifest.OpenPGP)
			d.credentialMu.Unlock()
		}
	case "credentials_release":
		d.clearCredentialProviderSources(req.Host, request.Workspace)
	}
}

func (d *Daemon) openPGPManifestForClaim(ctx context.Context, host string, hello credentialStreamHello, bundle CredentialBundleConfig) (*credentialOpenPGPManifest, error) {
	key := credentialSSHSourceKey(host, hello.Workspace, hello.Generation)
	d.credentialMu.Lock()
	manifest := cloneCredentialOpenPGPManifest(d.credentialOpenPGP[key])
	d.credentialMu.Unlock()
	if manifest != nil {
		return manifest, nil
	}
	d.mu.Lock()
	remote := d.state.Remotes[host]
	var claimedKeys []string
	if remote != nil && remote.Workspaces[hello.Workspace] != nil && remote.Workspaces[hello.Workspace].CredentialClaim != nil {
		claimedKeys = append(claimedKeys, remote.Workspaces[hello.Workspace].CredentialClaim.OpenPGPKeys...)
	}
	d.mu.Unlock()
	if !sameStringSet(bundle.OpenPGP.SigningKeys, claimedKeys) {
		return nil, fmt.Errorf("OpenPGP provider keys changed; release and re-claim the credential bundle")
	}
	resolved, err := buildOpenPGPManifest(ctx, d.config, bundle.OpenPGP.SigningKeys, d.providerRunner())
	if err != nil {
		return nil, err
	}
	d.credentialMu.Lock()
	if d.credentialOpenPGP == nil {
		d.credentialOpenPGP = map[string]*credentialOpenPGPManifest{}
	}
	d.credentialOpenPGP[key] = cloneCredentialOpenPGPManifest(resolved)
	d.credentialMu.Unlock()
	return resolved, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]bool, len(left))
	for _, value := range left {
		values[value] = true
	}
	for _, value := range right {
		if !values[value] {
			return false
		}
	}
	return len(values) == len(left)
}

func credentialOperationKey(workspace, capability, operation string) string {
	return workspace + "\x00" + capability + "\x00" + operation
}

func (d *Daemon) beginCredentialOperation(workspace, capability, operation string) func() {
	key := credentialOperationKey(workspace, capability, operation)
	d.credentialMu.Lock()
	d.credentialActive[key]++
	d.credentialMu.Unlock()
	return func() {
		d.credentialMu.Lock()
		if d.credentialActive[key] <= 1 {
			delete(d.credentialActive, key)
		} else {
			d.credentialActive[key]--
		}
		d.credentialMu.Unlock()
	}
}

func (d *Daemon) credentialActiveOperationStatus() []credentialActiveOperation {
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()
	result := make([]credentialActiveOperation, 0, len(d.credentialActive))
	for key, count := range d.credentialActive {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || count <= 0 {
			continue
		}
		result = append(result, credentialActiveOperation{WorkspaceID: parts[0], Capability: parts[1], Operation: parts[2], Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].WorkspaceID != result[j].WorkspaceID {
			return result[i].WorkspaceID < result[j].WorkspaceID
		}
		if result[i].Capability != result[j].Capability {
			return result[i].Capability < result[j].Capability
		}
		return result[i].Operation < result[j].Operation
	})
	return result
}

func (d *Daemon) allCredentialStatuses() credentialStatusResponse {
	d.mu.Lock()
	workspaces := make([]*Workspace, 0, len(d.state.Workspaces))
	for _, workspace := range d.state.Workspaces {
		workspaces = append(workspaces, workspace.Clone())
	}
	d.mu.Unlock()
	transport := d.credentialTransportStatus(workspaces)
	statuses := sortedCredentialStatuses(workspaces)
	byID := make(map[string]*Workspace, len(workspaces))
	for _, workspace := range workspaces {
		byID[workspace.ID] = workspace
	}
	for index := range statuses {
		workspace := byID[statuses[index].WorkspaceID]
		if workspace != nil && workspace.CredentialClaim != nil && !d.credentialClaimTransportReady(workspace) {
			degradeCredentialStatus(&statuses[index], workspace)
		}
		d.degradeMissingCredentialSSHSource(&statuses[index], workspace)
		d.degradeMissingCredentialRoutes(&statuses[index], workspace)
	}
	return credentialStatusResponse{
		Transport:        transport,
		Workspaces:       statuses,
		ActiveOperations: d.credentialActiveOperationStatus(),
	}
}

func (d *Daemon) degradeMissingCredentialRoutes(status *workspaceCredentialStatus, workspace *Workspace) {
	if status == nil || workspace == nil || workspace.RemoteHost != "" || workspace.CredentialClaim == nil {
		return
	}
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()
	for name, capability := range status.Capabilities {
		listener := d.credentialRoutes[credentialRouteKey(workspace.ID, name)]
		if listener != nil && listener.socketPublished() {
			continue
		}
		status.State = "degraded"
		capability.State = "unavailable"
		capability.Available = false
		capability.Detail = appendCredentialDetail(capability.Detail, "stable workspace credential route is not published; reconciliation will retry")
		status.Capabilities[name] = capability
	}
}

func (d *Daemon) degradeMissingCredentialSSHSource(status *workspaceCredentialStatus, workspace *Workspace) {
	if status == nil || workspace == nil || workspace.CredentialClaim == nil ||
		workspace.CredentialClaim.OwnerNodeID != d.state.Node.ID || d.sshAgent.IdentityAgent != "" {
		return
	}
	if _, enabled := status.Capabilities[credentialCapabilitySSH]; !enabled {
		return
	}
	host := workspace.RemoteHost
	if host == "" && workspace.CredentialClaim.ProviderSource != "local" {
		return
	}
	key := credentialSSHSourceKey(host, workspace.ID, workspace.CredentialClaim.Generation)
	d.credentialMu.Lock()
	socket := d.credentialSSHSources[key]
	d.credentialMu.Unlock()
	if socket != "" {
		return
	}
	status.State = "degraded"
	capability := status.Capabilities[credentialCapabilitySSH]
	capability.State = "unavailable"
	capability.Available = false
	if host == "" {
		capability.Detail = "local credential source is unavailable; reactivate local credentials to restore the SSH agent"
	} else {
		capability.Detail = "provider restarted without ssh.identity_agent; re-claim credentials to restore the attaching user's agent"
	}
	status.Capabilities[credentialCapabilitySSH] = capability
}

func (d *Daemon) setIncomingCredentialTransport(req credentialTransportSessionRequest) error {
	if req.Provider.ID == "" {
		return fmt.Errorf("credential transport provider id is required")
	}
	if req.State != "ready" && req.State != "disconnected" {
		return fmt.Errorf("invalid credential transport state %q", req.State)
	}
	if req.State == "ready" {
		if req.Endpoint == "" || !filepath.IsAbs(req.Endpoint) {
			return fmt.Errorf("credential transport endpoint must be an absolute path")
		}
		info, err := os.Lstat(req.Endpoint)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("credential transport endpoint %q is not a Unix socket", req.Endpoint)
		}
	}
	d.credentialMu.Lock()
	if req.State == "disconnected" {
		current, ok := d.credentialTransports[req.Provider.ID]
		if ok && req.Endpoint != "" && current.Endpoint != req.Endpoint {
			d.credentialMu.Unlock()
			return nil
		}
	}
	d.credentialTransports[req.Provider.ID] = incomingCredentialTransport{
		Provider: req.Provider, State: req.State, Endpoint: req.Endpoint, UpdatedAt: time.Now().UTC(),
	}
	d.credentialMu.Unlock()
	return nil
}

func (d *Daemon) incomingCredentialTransportReady(nodeID string) bool {
	d.credentialMu.Lock()
	transport, ok := d.credentialTransports[nodeID]
	d.credentialMu.Unlock()
	return ok && transport.State == "ready" && time.Since(transport.UpdatedAt) <= 3*time.Second
}

func (d *Daemon) credentialClaimTransportReady(workspace *Workspace) bool {
	if workspace == nil || workspace.CredentialClaim == nil {
		return true
	}
	if workspace.CredentialClaim.ProviderSource == "local" {
		return true
	}
	if workspace.RemoteHost != "" {
		return d.remotes.credentialTransportStatusForHost(workspace.RemoteHost).State == "ready"
	}
	return d.incomingCredentialTransportReady(workspace.CredentialClaim.OwnerNodeID)
}

func (d *Daemon) credentialTransportStatus(workspaces []*Workspace) credentialTransportView {
	outbound := d.remotes.credentialTransportStatus()
	inboundReady := false
	inboundUnavailable := false
	for _, workspace := range workspaces {
		if workspace.RemoteHost == "" && workspace.CredentialClaim != nil && workspace.CredentialClaim.ProviderSource != "local" {
			if d.incomingCredentialTransportReady(workspace.CredentialClaim.OwnerNodeID) {
				inboundReady = true
			} else {
				inboundUnavailable = true
			}
		}
	}
	if inboundUnavailable {
		return credentialTransportView{State: "degraded", LastError: "claim owner control session is unavailable"}
	}
	if inboundReady && outbound.State == "idle" {
		return credentialTransportView{State: "ready"}
	}
	return outbound
}

func degradeCredentialStatus(status *workspaceCredentialStatus, workspace *Workspace) {
	if workspace != nil && workspace.CredentialClaim != nil && workspace.CredentialClaim.ProviderSource == "local" {
		return
	}
	status.State = "degraded"
	for name, capability := range status.Capabilities {
		capability.State = "unavailable"
		capability.Available = false
		capability.Detail = "credential transport is unavailable; reconciliation will retry"
		status.Capabilities[name] = capability
	}
}

func (d *Daemon) remoteCredentialClaim(ctx context.Context, host string, hello credentialStreamHello) (*CredentialClaim, error) {
	lookup := func() *CredentialClaim {
		d.mu.Lock()
		defer d.mu.Unlock()
		remote := d.state.Remotes[host]
		if remote == nil || remote.Workspaces == nil {
			return nil
		}
		workspace := remote.Workspaces[hello.Workspace]
		if workspace == nil || workspace.CredentialClaim == nil {
			return nil
		}
		return workspace.CredentialClaim
	}
	claim := lookup()
	if claim == nil || claim.Generation != hello.Generation || claim.Bundle != hello.Bundle {
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, err := d.remotes.Call(callCtx, host, "get", encodeRemotePayload(refRequest{Ref: hello.Workspace})); err != nil {
			return nil, errCredentialUnavailable
		}
		claim = lookup()
	}
	if claim == nil || claim.Generation != hello.Generation || claim.Bundle != hello.Bundle || claim.OwnerNodeID != d.state.Node.ID {
		return nil, errCredentialUnavailable
	}
	if _, ok := claim.Capabilities[hello.Capability]; !ok {
		return nil, errCredentialUnavailable
	}
	return claim, nil
}

func encodeRemotePayload(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func proxyCredentialConnections(ctx context.Context, left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	select {
	case <-ctx.Done():
	case <-done:
	}
}

var errCredentialUnavailable = errors.New("credential capability is unavailable")
