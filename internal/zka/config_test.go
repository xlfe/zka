package zka

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAttentionConfigDefaultsAndExplicitNotificationDisable(t *testing.T) {
	t.Setenv("ZKA_CONFIG", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Attention.States, []AgentState{StateBlocked, StateError, StateDone}) ||
		!cfg.Notifications.DesktopEnabled || !cfg.Notifications.NtfyEnabled || cfg.Notifications.NtfyIncludeEvidence ||
		!cfg.Integrations.CodexManagedHooks || !cfg.Integrations.ClaudeManagedHooks {
		t.Fatalf("defaults = %#v", cfg)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"notifications":{"desktop_enabled":false,"ntfy_enabled":false,"ntfy_include_evidence":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.DesktopEnabled || cfg.Notifications.NtfyEnabled || !cfg.Notifications.NtfyIncludeEvidence {
		t.Fatalf("explicit channel disable was ignored: %#v", cfg.Notifications)
	}
}

func TestManagedHookIntegrationsCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"integrations":{"codex_managed_hooks":false,"claude_managed_hooks":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Integrations.CodexManagedHooks || cfg.Integrations.ClaudeManagedHooks {
		t.Fatalf("managed hooks remained enabled: %#v", cfg.Integrations)
	}
}

func TestAttentionConfigRejectsUnsupportedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"attention":{"states":["working"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "unsupported state") {
		t.Fatalf("error = %v", err)
	}
}

func TestSSHIdentityAgentPrecedesOtherOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"ssh":{"identity_agent":"/run/user/%i/ssh-agent.socket","options":["-o","BatchMode=yes"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.SSH.Options, " ")
	if !strings.HasPrefix(joined, "-o IdentityAgent=/run/user/%i/ssh-agent.socket ") {
		t.Fatalf("ssh options = %q", joined)
	}
}

func TestCredentialBundlesReplaceForwardAgentConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"ssh":{"forward_agent":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("legacy forward_agent error = %v", err)
	}
	for _, options := range []string{
		`["-A"]`,
		`["-o","ForwardAgent=yes"]`,
		`["-oForwardAgent=yes"]`,
	} {
		if err := os.WriteFile(path, []byte(`{"ssh":{"options":`+options+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "must not enable ForwardAgent") {
			t.Fatalf("ForwardAgent options %s error = %v", options, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"credentials":{"default_bundle":"work","bundles":{"work":{"ssh_agent":{"enable":true}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Credentials.DefaultBundle != "work" || !cfg.Credentials.Bundles["work"].SSHAgent.Enable || strings.Contains(strings.Join(cfg.SSH.Options, " "), "ForwardAgent") {
		t.Fatalf("credentials config = %#v", cfg.Credentials)
	}
}

func TestConfigRejectsUnimplementedPIVBProvenanceMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credentials":{"pivb":{"routing_mode":"cgroup-bound"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), `must be "environment"`) || !strings.Contains(err.Error(), "enforced provenance is not available yet") {
		t.Fatalf("unimplemented provenance mode error = %v", err)
	}
}

func TestOpenPGPTargetBundleDoesNotRequireProviderFingerprints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credentials":{"bundles":{"work":{"openpgp":{"enable":true}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Credentials.Bundles["work"].OpenPGP.Enable || len(cfg.Credentials.Bundles["work"].OpenPGP.SigningKeys) != 0 {
		t.Fatalf("target-only OpenPGP bundle = %#v", cfg.Credentials.Bundles["work"])
	}
}

func TestCredentialProviderIdentityConfigIsValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	valid := `{"ssh":{"expected_node_ids":{"devbox":"0123456789abcdef0123456789abcdef"}},"credentials":{"providers":{"laptop":{"node_id":"fedcba9876543210fedcba9876543210","ssh_source_addresses":["192.0.2.10","2001:db8::/64"]},"roaming":{"node_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSH.ExpectedNodeIDs["devbox"] == "" || len(cfg.Credentials.Providers["laptop"].SSHSourceAddresses) != 2 ||
		len(cfg.Credentials.Providers["roaming"].SSHSourceAddresses) != 0 {
		t.Fatalf("identity config = %#v", cfg)
	}
	for _, invalid := range []string{
		`{"ssh":{"expected_node_ids":{"devbox":"NOT-A-NODE"}}}`,
		`{"credentials":{"providers":{"laptop":{"node_id":"fedcba9876543210fedcba9876543210","ssh_source_addresses":["not-an-address"]}}}}`,
	} {
		if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("invalid identity config accepted: %s", invalid)
		}
	}
}

func TestSSHAgentInfoExpandsUIDAndHintsOnlyForAuthentication(t *testing.T) {
	var cfg Config
	cfg.SSH.IdentityAgent = "/run/user/%i/ssh-agent.socket"
	agent := newSSHAgentInfo(cfg, "/run/user/1234/agent-a.socket")
	if strings.Contains(agent.EffectiveSocket, "%i") || !strings.HasSuffix(agent.EffectiveSocket, "/ssh-agent.socket") {
		t.Fatalf("agent info = %#v", agent)
	}
	authErr := errors.New("Permission denied (publickey)")
	hinted := withSSHAgentMismatchHint(authErr, agent, "/run/user/1234/agent-a.socket")
	if !strings.Contains(hinted.Error(), "SSH agent mismatch") {
		t.Fatalf("hinted error = %v", hinted)
	}
	plainErr := errors.New("connection refused")
	if got := withSSHAgentMismatchHint(plainErr, agent, "/different/agent"); got != plainErr {
		t.Fatalf("non-authentication error changed: %v", got)
	}
	var optionConfig Config
	optionConfig.SSH.Options = []string{"-o", "IdentityAgent=/run/user/%i/option-agent", "-o", "BatchMode=yes"}
	optionAgent := newSSHAgentInfo(optionConfig, "/inherited")
	if optionAgent.IdentityAgent != "/run/user/%i/option-agent" || strings.Contains(optionAgent.EffectiveSocket, "%i") {
		t.Fatalf("agent selected through ssh options = %#v", optionAgent)
	}
}

func TestHeadlessConfigDefaultsFileAndEnvOverride(t *testing.T) {
	t.Setenv("ZKA_CONFIG", "")
	t.Setenv("ZKA_HEADLESS", "")
	cfg, err := LoadConfig()
	if err != nil || cfg.Headless {
		t.Fatalf("default headless = %v, %v", cfg.Headless, err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"headless":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err = LoadConfig()
	if err != nil || !cfg.Headless {
		t.Fatalf("file headless = %v, %v", cfg.Headless, err)
	}
	// The bare-name view-layer commands stay valid under headless.
	if cfg.Kitty.Command != "kitty" || cfg.Focus.SwayCommand != "swaymsg" {
		t.Fatalf("headless clobbered view-layer commands: %#v", cfg)
	}
	if err := os.WriteFile(path, []byte(`{"headless":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_HEADLESS", "1")
	cfg, err = LoadConfig()
	if err != nil || !cfg.Headless {
		t.Fatalf("ZKA_HEADLESS override = %v, %v", cfg.Headless, err)
	}
}
