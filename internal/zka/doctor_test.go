package zka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentPaneCredentialEnvironmentDoctorCheck(t *testing.T) {
	paths := testPaths(testRoot(t))
	t.Setenv("ZKA_PANE_ID", "")
	t.Setenv("ZKA_WORKSPACE_ID", "")
	t.Setenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "")
	if check, unsafe := currentPaneCredentialEnvironmentDoctorCheck(paths); !check.OK || unsafe || !strings.Contains(check.Detail, "not running") {
		t.Fatalf("outside pane check = %#v unsafe=%v", check, unsafe)
	}

	t.Setenv("ZKA_PANE_ID", "pane")
	t.Setenv("ZKA_WORKSPACE_ID", "workspace")
	if check, unsafe := currentPaneCredentialEnvironmentDoctorCheck(paths); check.OK || !unsafe || !strings.Contains(check.Detail, "version 0") {
		t.Fatalf("version-0 pane check = %#v unsafe=%v", check, unsafe)
	}

	t.Setenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "2")
	check, unsafe := currentPaneCredentialEnvironmentDoctorCheck(paths)
	if !check.OK || unsafe || !strings.Contains(check.Detail, "managed credential environment v2") {
		t.Fatalf("v2 managed pane check = %#v unsafe=%v", check, unsafe)
	}

	t.Setenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "3")
	if check, unsafe = currentPaneCredentialEnvironmentDoctorCheck(paths); !check.OK || unsafe || !strings.Contains(check.Detail, "managed credential environment v3") {
		t.Fatalf("remote pane check = %#v unsafe=%v", check, unsafe)
	}
}

func TestDoctorLegacyPaneSkipsContaminatedProviderChecks(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	cfg := defaultConfig()
	cfg.SSH.IdentityAgent = "/provider/socket/must-not-be-probed"
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	cfg.Credentials.Bundles["work"] = bundle
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", configPath)
	t.Setenv("ZKA_PANE_ID", "pane")
	t.Setenv("ZKA_WORKSPACE_ID", "workspace")
	t.Setenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "0")

	var stdout, stderr bytes.Buffer
	code, err := runDoctor(nil, d.paths, &stdout, &stderr)
	if err != nil || code != 1 {
		t.Fatalf("doctor code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "credentials-provider skipped: the current pane") || strings.Contains(stdout.String(), "/provider/socket/must-not-be-probed") {
		t.Fatalf("legacy doctor output = %s", stdout.String())
	}
}

func TestCredentialEnvironmentInventoryDoctorCheck(t *testing.T) {
	status := credentialStatusResponse{Workspaces: []workspaceCredentialStatus{
		{WorkspaceName: "local", State: "unclaimed", RecreatePaneIDs: []string{"pane-b", "pane-a"}, RecreationDetail: "legacy migration"},
	}}
	check := credentialEnvironmentInventoryDoctorCheck(status, nil)
	if check.OK || !strings.Contains(check.Detail, "local=pane-b,pane-a") || !strings.Contains(check.Detail, "legacy migration") {
		t.Fatalf("inventory check = %#v", check)
	}
	if healthy := credentialEnvironmentInventoryDoctorCheck(credentialStatusResponse{}, nil); !healthy.OK {
		t.Fatalf("healthy inventory check = %#v", healthy)
	}
}

func TestDoctorOriginReportsCredentialChecks(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = "/definitely/missing/zka-ssh"
	serveTestDaemon(t, d)
	var stdout, stderr bytes.Buffer
	_, err = runDoctor([]string{"--origin", "devbox.example"}, d.paths, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"credentials-config", "credentials-provider", "credentials-claim", "credentials-transport", "openpgp-keys"} {
		if !bytes.Contains(stdout.Bytes(), []byte(expected)) {
			t.Fatalf("doctor output missing %q: %s", expected, stdout.String())
		}
	}
}

func TestDoctorWarningIsLoudWithoutFailing(t *testing.T) {
	var stdout bytes.Buffer
	code, err := writeDoctorResult([]doctorCheck{{
		Name: "openpgp-keys", OK: true, Warning: true,
		Detail: "work=11112222…99990000/A1B2C3D4:software",
	}}, false, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("warning exit code = %d, want 0", code)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("WARN  openpgp-keys")) || !bytes.Contains(stdout.Bytes(), []byte(":software")) {
		t.Fatalf("warning output = %q", stdout.String())
	}
}

func TestDoctorSSHAgentChecksCompareFingerprints(t *testing.T) {
	identities := map[string][]string{
		"/daemon": {"SHA256:daemon"},
		"/caller": {"SHA256:caller"},
		"/same":   {"SHA256:daemon"},
	}
	inspect := func(_ context.Context, socket string) ([]string, error) {
		return identities[socket], nil
	}
	daemon := sshAgentInfo{InheritedSocket: "/daemon", EffectiveSocket: "/daemon"}

	different := doctorSSHAgentChecks(context.Background(), daemon, "/caller", inspect)
	if len(different) != 3 || different[2].OK || !bytes.Contains([]byte(different[2].Detail), []byte("different identities")) {
		t.Fatalf("different-agent checks = %#v", different)
	}
	same := doctorSSHAgentChecks(context.Background(), daemon, "/same", inspect)
	if len(same) != 3 || !same[2].OK || !bytes.Contains([]byte(same[2].Detail), []byte("same identities")) {
		t.Fatalf("same-identity checks = %#v", same)
	}
}

func TestSSHPublicKeyFingerprints(t *testing.T) {
	fingerprints, err := sshPublicKeyFingerprints("ssh-ed25519 aGVsbG8= fixture-key\n")
	if err != nil {
		t.Fatal(err)
	}
	const want = "SHA256:LPJNul+wow4m6DsqxbninhsWHlwfp0JecwQzYpOLmCQ"
	if len(fingerprints) != 1 || fingerprints[0] != want {
		t.Fatalf("fingerprints = %#v", fingerprints)
	}
}

func TestManagedHookDoctorCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"command":"zka hook claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := managedHookDoctorCheck("claude-hooks", path, "hook claude", true); !check.OK || check.Detail != path {
		t.Fatalf("installed check = %#v", check)
	}
	if check := managedHookDoctorCheck("claude-hooks", path, "hook codex", true); check.OK || !bytes.Contains([]byte(check.Detail), []byte("not found")) {
		t.Fatalf("missing command check = %#v", check)
	}
	if check := managedHookDoctorCheck("claude-hooks", "/missing", "hook claude", false); !check.OK || !bytes.Contains([]byte(check.Detail), []byte("disabled")) {
		t.Fatalf("disabled check = %#v", check)
	}
}

func TestSwayIPCDoctorCheck(t *testing.T) {
	probeCalls := 0
	probe := func(context.Context) (swaySocketInfo, error) {
		probeCalls++
		return swaySocketInfo{Path: "/run/user/1234/sway.sock", Source: "XDG_RUNTIME_DIR"}, nil
	}

	headless := swayIPCDoctorCheck(context.Background(), true, true, probe)
	if !headless.OK || headless.Name != "sway-ipc" || !bytes.Contains([]byte(headless.Detail), []byte("headless")) {
		t.Fatalf("headless check = %#v", headless)
	}
	disabled := swayIPCDoctorCheck(context.Background(), false, false, probe)
	if !disabled.OK || !bytes.Contains([]byte(disabled.Detail), []byte("disabled")) {
		t.Fatalf("disabled check = %#v", disabled)
	}
	if probeCalls != 0 {
		t.Fatalf("probe calls for skipped checks = %d, want 0", probeCalls)
	}

	connected := swayIPCDoctorCheck(context.Background(), false, true, probe)
	if !connected.OK || connected.Warning || connected.Detail != "/run/user/1234/sway.sock via XDG_RUNTIME_DIR" {
		t.Fatalf("connected check = %#v", connected)
	}
	if probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", probeCalls)
	}

	recovered := swayIPCDoctorCheck(context.Background(), false, true, func(context.Context) (swaySocketInfo, error) {
		return swaySocketInfo{
			Path: "/run/user/1234/sway-ipc.1000.99.sock", Source: "XDG_RUNTIME_DIR",
			FailedAttempts: []swaySocketAttempt{{
				Path: "/run/user/1234/sway-ipc.1000.42.sock", Source: "SWAYSOCK", Error: "unable to connect",
			}},
		}, nil
	})
	if !recovered.OK || !recovered.Warning || !strings.Contains(recovered.Detail, "recovered via XDG_RUNTIME_DIR") ||
		!strings.Contains(recovered.Detail, "SWAYSOCK=/run/user/1234/sway-ipc.1000.42.sock is stale") ||
		!strings.Contains(recovered.Detail, "fix your Sway session environment import") ||
		!strings.Contains(recovered.Detail, "does not repair other programs") {
		t.Fatalf("recovered check = %#v", recovered)
	}

	runtimeRecovery := swayIPCDoctorCheck(context.Background(), false, true, func(context.Context) (swaySocketInfo, error) {
		return swaySocketInfo{
			Path: "/run/user/1234/sway-ipc.1000.99.sock", Source: "XDG_RUNTIME_DIR",
			FailedAttempts: []swaySocketAttempt{{
				Path: "/run/user/1234/sway-ipc.1000.98.sock", Source: "XDG_RUNTIME_DIR", Error: "unable to connect",
			}},
		}, nil
	})
	if !runtimeRecovery.OK || runtimeRecovery.Warning || runtimeRecovery.Detail != "/run/user/1234/sway-ipc.1000.99.sock via XDG_RUNTIME_DIR" {
		t.Fatalf("runtime recovery check = %#v", runtimeRecovery)
	}

	failed := swayIPCDoctorCheck(context.Background(), false, true, func(context.Context) (swaySocketInfo, error) {
		return swaySocketInfo{}, errors.New("Unable to retrieve socket path")
	})
	if failed.OK || !bytes.Contains([]byte(failed.Detail), []byte("Unable to retrieve socket path")) {
		t.Fatalf("failed check = %#v", failed)
	}

	oldDaemon := swayIPCDoctorCheck(context.Background(), false, true, func(context.Context) (swaySocketInfo, error) {
		return swaySocketInfo{}, errors.New(`unknown operation "sway_ipc"`)
	})
	if oldDaemon.OK || !bytes.Contains([]byte(oldDaemon.Detail), []byte("restart zkad after upgrading")) {
		t.Fatalf("old-daemon check = %#v", oldDaemon)
	}
}

func TestDoctorHeadlessSkipsViewLayerChecks(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"headless":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	var stdout, stderr bytes.Buffer
	if _, err := runDoctor(nil, d.paths, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	// kitty, kitten, swaymsg, and kitty-watcher are skipped by configuration;
	// zmx/ssh/ntfy-send and credential checks must still be real lookups.
	if got := bytes.Count(stdout.Bytes(), []byte("skipped on a headless origin")); got != 5 {
		t.Fatalf("skip count = %d in:\n%s", got, out)
	}
}
