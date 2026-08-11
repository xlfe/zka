package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type pivbCapabilityRunner struct {
	capabilities string
	err          error
	calls        [][]string
}

func (r *pivbCapabilityRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) != 0 && args[0] == "version" {
		return "1.2.3\n", "", nil
	}
	return r.capabilities, "probe failed", r.err
}

func pivbCapabilityConfig(mode string) Config {
	cfg := defaultConfig()
	cfg.Credentials.PIVB.RoutingMode = mode
	cfg.Credentials.Bundles["work"] = CredentialBundleConfig{}
	bundle := cfg.Credentials.Bundles["work"]
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	cfg.Credentials.Bundles["work"] = bundle
	return cfg
}

func TestManagedPIVBCapabilityNegotiatesCooperativeProtocol(t *testing.T) {
	envelope := `{"schema":1,"attachment_protocols":[1],"attachment_modes":["local-allowed","route-required"],"future":true}`
	runner := &pivbCapabilityRunner{capabilities: envelope}
	if err := ensureManagedPIVBCapability(context.Background(), pivbCapabilityConfig(pivbRoutingEnvironment), runner); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPIVBCapabilityFailsClosed(t *testing.T) {
	tests := []struct {
		name, mode, envelope string
		err                  error
		want                 string
	}{
		{"old binary", pivbRoutingEnvironment, "", errors.New("unknown command"), "upgrade PIVB"},
		{"missing protocol", pivbRoutingEnvironment, `{"schema":1,"attachment_protocols":[],"attachment_modes":["route-required"]}`, nil, "protocol 1"},
		{"unsupported protocol", pivbRoutingEnvironment, `{"schema":1,"attachment_protocols":[2],"attachment_modes":["route-required"]}`, nil, "protocol 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &pivbCapabilityRunner{capabilities: test.envelope, err: test.err}
			err := ensureManagedPIVBCapability(context.Background(), pivbCapabilityConfig(test.mode), runner)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
