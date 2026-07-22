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
	if !reflect.DeepEqual(cfg.Attention.States, []AgentState{StateBlocked, StateError, StateDone}) || !cfg.Notifications.DesktopEnabled || !cfg.Notifications.NtfyEnabled {
		t.Fatalf("defaults = %#v", cfg)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"notifications":{"desktop_enabled":false,"ntfy_enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.DesktopEnabled || cfg.Notifications.NtfyEnabled {
		t.Fatalf("explicit channel disable was ignored: %#v", cfg.Notifications)
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

func TestSSHForwardAgentIsOptInAndPrecedesOtherOptions(t *testing.T) {
	t.Setenv("ZKA_CONFIG", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSH.ForwardAgent || strings.Contains(strings.Join(cfg.SSH.Options, " "), "ForwardAgent") {
		t.Fatalf("forwarding enabled by default: %#v", cfg.SSH)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"ssh":{"forward_agent":true,"options":["-o","ForwardAgent=no","-o","BatchMode=yes"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", path)
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SSH.ForwardAgent || !strings.HasPrefix(strings.Join(cfg.SSH.Options, " "), "-o ForwardAgent=yes ") {
		t.Fatalf("forwarding options = %#v", cfg.SSH)
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
