package zka

import (
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
