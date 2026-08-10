package zka

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type credentialMigrationRunner struct {
	mu          sync.Mutex
	active      map[string]bool
	calls       []runnerCall
	environment []string
	directory   string
	onStart     func()
	helpErr     error
	killErr     error
	startErr    error
}

func (r *credentialMigrationRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runnerCall{Name: name, Args: append([]string(nil), args...)})
	joined := strings.Join(args, " ")
	switch {
	case joined == "help":
		return "[r]un <name> [-d] [command...]\n", "", r.helpErr
	case joined == "list --short":
		var sessions []string
		for session, active := range r.active {
			if active {
				sessions = append(sessions, session)
			}
		}
		return strings.Join(sessions, "\n"), "", nil
	case len(args) == 3 && args[0] == "kill" && args[2] == "--force":
		if r.killErr != nil {
			return "", r.killErr.Error(), r.killErr
		}
		r.active[args[1]] = false
		return "", "", nil
	default:
		return "", "", nil
	}
}

func (r *credentialMigrationRunner) RunConfigured(_ context.Context, name string, args []string, environment []string, directory string) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, runnerCall{Name: name, Args: append([]string(nil), args...)})
	r.environment = append([]string(nil), environment...)
	r.directory = directory
	if r.startErr != nil {
		err := r.startErr
		r.mu.Unlock()
		return "", err.Error(), err
	}
	if len(args) > 1 {
		r.active[args[1]] = true
	}
	onStart := r.onStart
	r.mu.Unlock()
	if onStart != nil {
		onStart()
	}
	return "", "", nil
}

func TestVersionZeroPaneMigrationBindsLocalDefaultBeforeDetachedRestart(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}}
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := listenUnix(d.paths.RuntimeDir + "/migration-agent.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	go func() {
		for {
			conn, acceptErr := agent.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	d.config.Credentials.DefaultBundle = "work"
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.sshAgent = sshAgentInfo{EffectiveSocket: d.paths.RuntimeDir + "/migration-agent.sock"}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	current := d.state.Workspaces[workspace.ID].Panes[pane.ID]
	current.BackendCreated, current.BackendReady = true, true
	current.Process = ProcessStatus{Running: true, PID: 42}
	d.mu.Unlock()
	runner.active[pane.Backend.Ref] = true
	runner.onStart = func() {
		_, eventErr := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "pane-host",
			PID: 99, CredentialEnvironmentVersion: credentialEnvironmentVersion,
		})
		if eventErr != nil {
			t.Errorf("report replacement process: %v", eventErr)
		}
	}

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Clone()
	d.mu.Unlock()
	if result.CredentialClaim == nil || result.CredentialClaim.ProviderSource != "local" || result.CredentialClaim.OwnerNodeID != d.state.Node.ID {
		t.Fatalf("migration binding = %#v", result.CredentialClaim)
	}
	migrated := result.Panes[pane.ID]
	if migrated.CredentialEnvironmentVersion != credentialEnvironmentVersion || migrated.CredentialMigrationState != "" || !migrated.BackendCreated || migrated.Process.PID != 99 {
		t.Fatalf("migrated pane = %#v", migrated)
	}
	if got := testEnvironmentValue(runner.environment, "SSH_AUTH_SOCK"); got != agentRelaySocketPath(d.paths.AgentDir, workspace.ID) {
		t.Fatalf("replacement SSH_AUTH_SOCK = %q", got)
	}
	if got := testEnvironmentValue(runner.environment, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"); got != "4" {
		t.Fatalf("replacement credential version = %q", got)
	}
	if !migrationRunnerCalled(runner.calls, "kill", pane.Backend.Ref, "--force") ||
		!migrationRunnerCalled(runner.calls, "run", pane.Backend.Ref, "-d", "zka", "pane-host") {
		t.Fatalf("migration calls = %#v", runner.calls)
	}
}

func TestVersionZeroPaneMigrationDoesNotStopUnclaimedWorkspaceWithoutDefault(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}}
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].Panes[pane.ID].BackendCreated = true
	d.mu.Unlock()
	runner.active[pane.Backend.Ref] = true

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if !result.BackendCreated || result.BackendDead || !strings.Contains(result.CredentialMigrationError, "default_bundle") {
		t.Fatalf("blocked migration pane = %#v", result)
	}
	if migrationRunnerCalled(runner.calls, "kill", pane.Backend.Ref, "--force") {
		t.Fatalf("unclaimed migration stopped the original backend: %#v", runner.calls)
	}
}

func TestVersionZeroPaneMigrationDoesNotStopWhenRemoteProviderIsUnavailable(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}}
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	current := d.state.Workspaces[workspace.ID]
	current.CredentialClaim = &CredentialClaim{
		ProviderSource: "remote", Bundle: "work", OwnerNodeID: "offline-provider", Generation: 1, State: "ready",
		Capabilities: map[string]CredentialCapabilityStatus{credentialCapabilitySSH: {State: "ready", Available: true}},
	}
	current.Panes[pane.ID].BackendCreated = true
	d.mu.Unlock()
	runner.active[pane.Backend.Ref] = true

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if !result.BackendCreated || result.BackendDead || !strings.Contains(result.CredentialMigrationError, "binding generation") {
		t.Fatalf("unavailable-provider migration pane = %#v", result)
	}
	if migrationRunnerCalled(runner.calls, "kill", pane.Backend.Ref, "--force") {
		t.Fatalf("unavailable provider migration stopped the original backend: %#v", runner.calls)
	}
}

func TestVersionZeroPaneMigrationStartFailureRemainsRetryableAndNotReclaimed(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}, startErr: errors.New("no space left on device")}
	d, workspace, pane := versionZeroMigrationFixture(t, runner)

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	failed := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if failed.CredentialMigrationState != credentialMigrationPending || failed.BackendDead || failed.BackendCreated || !failed.BackendStart ||
		failed.Evidence.Event != "credential_environment_migration_failed" || !strings.Contains(failed.CredentialMigrationError, "no space left") {
		t.Fatalf("failed replacement = %#v", failed)
	}
	reconciled, err := d.reconcileBackends(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.Deleted) != 0 {
		t.Fatalf("failed migration reclaimed workspace: %#v", reconciled)
	}
	if _, err := d.getWorkspace(workspace.ID); err != nil {
		t.Fatalf("workspace disappeared after failed migration: %v", err)
	}

	runner.mu.Lock()
	runner.startErr = nil
	runner.onStart = func() {
		_, eventErr := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "pane-host",
			PID: 99, CredentialEnvironmentVersion: credentialEnvironmentVersion,
		})
		if eventErr != nil {
			t.Errorf("report replacement process: %v", eventErr)
		}
	}
	runner.mu.Unlock()
	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	retried := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if retried.CredentialMigrationState != "" || retried.CredentialEnvironmentVersion != credentialEnvironmentVersion || !retried.BackendCreated || retried.BackendDead {
		t.Fatalf("retried replacement = %#v", retried)
	}
}

func TestVersionZeroPaneMigrationKillFailureLeavesOriginalBackendLive(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}, killErr: errors.New("kill refused")}
	d, workspace, pane := versionZeroMigrationFixture(t, runner)

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if !result.BackendCreated || result.BackendDead || result.CredentialMigrationState != credentialMigrationFailed || !strings.Contains(result.CredentialMigrationError, "kill refused") {
		t.Fatalf("kill failure pane = %#v", result)
	}
	if !runner.active[pane.Backend.Ref] || migrationRunnerCalled(runner.calls, "run", pane.Backend.Ref) {
		t.Fatalf("kill failure calls = %#v active=%#v", runner.calls, runner.active)
	}
}

func TestVersionZeroPaneMigrationDetachedPreflightFailureDoesNotKillBackend(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}, helpErr: errors.New("zmx help failed")}
	d, workspace, pane := versionZeroMigrationFixture(t, runner)

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if !result.BackendCreated || result.BackendDead || !strings.Contains(result.CredentialMigrationError, "detached zmx") {
		t.Fatalf("preflight failure pane = %#v", result)
	}
	if !runner.active[pane.Backend.Ref] || migrationRunnerCalled(runner.calls, "kill", pane.Backend.Ref) {
		t.Fatalf("preflight failure calls = %#v active=%#v", runner.calls, runner.active)
	}
}

func TestVersionZeroPaneMigrationRecoversPendingPostKillState(t *testing.T) {
	runner := &credentialMigrationRunner{active: map[string]bool{}}
	d, workspace, pane := versionZeroMigrationFixture(t, runner)
	runner.active[pane.Backend.Ref] = false
	d.mu.Lock()
	current := d.state.Workspaces[workspace.ID].Panes[pane.ID]
	current.CredentialMigrationState = credentialMigrationPending
	current.BackendCreated, current.BackendReady, current.BackendStart, current.BackendDead = false, false, true, false
	d.mu.Unlock()
	runner.onStart = func() {
		_, eventErr := d.applyEvent(context.Background(), Event{
			WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "process_started", Source: "pane-host",
			PID: 100, CredentialEnvironmentVersion: credentialEnvironmentVersion,
		})
		if eventErr != nil {
			t.Errorf("report replacement process: %v", eventErr)
		}
	}

	d.migrateCredentialEnvironments(context.Background())
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if result.CredentialMigrationState != "" || result.CredentialEnvironmentVersion != credentialEnvironmentVersion || !result.BackendCreated {
		t.Fatalf("crash-recovered pane = %#v", result)
	}
}

func versionZeroMigrationFixture(t *testing.T, runner *credentialMigrationRunner) (*Daemon, *Workspace, *Pane) {
	t.Helper()
	d, err := newTestDaemon(t, testRoot(t), runner)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := listenUnix(d.paths.RuntimeDir + "/migration-fixture-agent.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	go func() {
		for {
			conn, acceptErr := agent.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	d.config.Credentials.DefaultBundle = "work"
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	d.sshAgent = sshAgentInfo{EffectiveSocket: d.paths.RuntimeDir + "/migration-fixture-agent.sock"}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	current := d.state.Workspaces[workspace.ID].Panes[pane.ID]
	current.BackendCreated, current.BackendReady = true, true
	current.Process = ProcessStatus{Running: true, PID: 42}
	d.mu.Unlock()
	runner.active[pane.Backend.Ref] = true
	return d, workspace, pane
}

func migrationRunnerCalled(calls []runnerCall, prefix ...string) bool {
	for _, call := range calls {
		if len(call.Args) < len(prefix) {
			continue
		}
		matched := true
		for index := range prefix {
			if call.Args[index] != prefix[index] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
