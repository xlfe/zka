package zka

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type credentialSessionRunnerCall struct {
	Name     string
	Args     []string
	Options  commandOptions
	Deadline time.Time
}

type credentialSessionRunner struct {
	mu      sync.Mutex
	calls   []credentialSessionRunnerCall
	handler func(context.Context, string, []string, commandOptions) (string, string, error)
}

func (r *credentialSessionRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return r.RunConfigured(ctx, name, args, commandOptions{})
}

func (r *credentialSessionRunner) RunConfigured(ctx context.Context, name string, args []string, options commandOptions) (string, string, error) {
	deadline, _ := ctx.Deadline()
	call := credentialSessionRunnerCall{
		Name: name, Args: append([]string(nil), args...), Deadline: deadline,
		Options: commandOptions{
			Environment: append([]string(nil), options.Environment...),
			Directory:   options.Directory,
			NewSession:  options.NewSession,
		},
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if r.handler != nil {
		return r.handler(ctx, name, args, options)
	}
	return "", "", nil
}

func (r *credentialSessionRunner) Calls() []credentialSessionRunnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]credentialSessionRunnerCall(nil), r.calls...)
}

func testCredentialSessionDaemon(runner CommandRunner, bundle CredentialBundleConfig) *Daemon {
	cfg := defaultConfig()
	cfg.Credentials.Bundles["work"] = bundle
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Daemon{
		config:                 cfg,
		runner:                 runner,
		paths:                  Paths{AgentDir: "/run/user/1000/zka/agents"},
		providerEnvironment:    []string{"PATH=/usr/bin", "DISPLAY=:9", "GPG_TTY=/dev/pts/1", "TERM=xterm-old", "LC_CTYPE=C", "LC_MESSAGES=C"},
		credentialSessionGate:  gate,
		credentialSessionProbe: func(map[string]string) error { return nil },
	}
}

func TestCredentialSessionRefreshPreservesExplicitLocaleWithoutTerminalState(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		switch args[0] {
		case "GETINFO std_env_names":
			// LC_CTYPE and LC_MESSAGES deliberately do not appear here. GnuPG
			// transports them as session OPTIONs outside std_env_names.
			return strings.Join([]string{
				"D GPG_TTY", "D TERM", "D WAYLAND_DISPLAY", "D DISPLAY",
				"D DBUS_SESSION_BUS_ADDRESS", "D INSIDE_EMACS", "OK",
			}, "\n"), "", nil
		case "updatestartuptty":
			return "OK\n", "", nil
		default:
			return "", "", errors.New("unexpected command")
		}
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{
		"WAYLAND_DISPLAY":          "wayland-7",
		"XDG_RUNTIME_DIR":          "/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
		"LC_CTYPE":                 "en_AU.UTF-8",
		"LC_MESSAGES":              "fr_FR.UTF-8",
		"GPG_TTY":                  "/dev/pts/9",
		"TERM":                     "xterm-kitty",
		"INSIDE_EMACS":             "29.4,comint",
	}})

	response := d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work", Action: "claim", Workspace: "workspace"})
	if response.State != "refreshed" {
		t.Fatalf("refresh response = %#v", response)
	}
	calls := runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %#v", calls)
	}
	update := calls[1]
	if !update.Options.NewSession {
		t.Fatal("UPDATESTARTUPTTY did not start in a new session")
	}
	environment := environmentMap(update.Options.Environment)
	if environment["LC_CTYPE"] != "en_AU.UTF-8" || environment["LC_MESSAGES"] != "fr_FR.UTF-8" {
		t.Fatalf("locale environment = %#v", environment)
	}
	for _, name := range []string{"GPG_TTY", "TERM", "INSIDE_EMACS"} {
		if _, present := environment[name]; present {
			t.Fatalf("terminal-specific %s survived refresh: %#v", name, environment)
		}
	}
	if environment["WAYLAND_DISPLAY"] != "wayland-7" || environment["XDG_RUNTIME_DIR"] != "/run/user/1000" {
		t.Fatalf("Wayland environment = %#v", environment)
	}
}

func TestOpenPGPAndSSHBundleDoesNotDependOnSSHAgentDiscovery(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.SSHAgent.Enable = true
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		switch args[0] {
		case "--list-dirs":
			return "", "", errors.New("transient gpgconf failure")
		case "GETINFO std_env_names":
			return "D DISPLAY\nOK\n", "", nil
		case "updatestartuptty":
			return "OK\n", "", nil
		default:
			return "", "", errors.New("unexpected command")
		}
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{"DISPLAY": ":1"}})
	response := d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work"})
	if response.State != "refreshed" || !response.Required {
		t.Fatalf("refresh response = %#v", response)
	}
	for _, call := range runner.Calls() {
		if len(call.Args) != 0 && call.Args[0] == "--list-dirs" {
			t.Fatalf("OpenPGP refresh probed the unrelated SSH agent: %#v", runner.Calls())
		}
	}
}

func TestCredentialSessionRefreshUsesOneTotalCommandBudget(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		switch args[0] {
		case "--list-dirs":
			return "/provider/S.gpg-agent.ssh\n", "", nil
		case "GETINFO std_env_names":
			return "D DISPLAY\nOK\n", "", nil
		case "updatestartuptty":
			return "OK\n", "", nil
		default:
			return "", "", errors.New("unexpected command")
		}
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	d.sshAgent = sshAgentInfo{EffectiveSocket: "/provider/S.gpg-agent.ssh"}
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{"DISPLAY": ":1"}})
	started := time.Now()
	response := d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work"})
	if response.State != "refreshed" {
		t.Fatalf("refresh response = %#v", response)
	}
	calls := runner.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %#v", calls)
	}
	wantDeadline := calls[0].Deadline
	if wantDeadline.IsZero() || wantDeadline.Sub(started) > credentialSessionRefreshTimeout+time.Second {
		t.Fatalf("first deadline = %v after %v", wantDeadline, started)
	}
	for _, call := range calls[1:] {
		if !call.Deadline.Equal(wantDeadline) {
			t.Fatalf("subprocess deadlines differ: %#v", calls)
		}
	}
}

func TestCredentialSessionRefreshReportsTotalBudgetExpiry(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	runner := &credentialSessionRunner{handler: func(ctx context.Context, _ string, _ []string, _ commandOptions) (string, string, error) {
		<-ctx.Done()
		return "", "", errors.New("signal: killed")
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{"DISPLAY": ":1"}})
	response := d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work"})
	if response.State != "warning" || !strings.Contains(response.Detail, "timed out") || strings.Contains(response.Detail, "signal: killed") {
		t.Fatalf("timeout response = %#v", response)
	}
}

func TestCredentialSessionRefreshSerializesConcurrentUpdates(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	getInfoCalls := 0
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		if args[0] == "GETINFO std_env_names" {
			mu.Lock()
			getInfoCalls++
			call := getInfoCalls
			mu.Unlock()
			if call == 1 {
				close(firstEntered)
				<-releaseFirst
			} else if call == 2 {
				close(secondEntered)
			}
			return "D DISPLAY\nOK\n", "", nil
		}
		if args[0] == "updatestartuptty" {
			return "OK\n", "", nil
		}
		return "", "", errors.New("unexpected command")
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{"DISPLAY": ":1"}})
	responses := make(chan credentialSessionRefreshResponse, 2)
	go func() {
		responses <- d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work", Action: "first"})
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not enter GETINFO")
	}
	go func() {
		responses <- d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work", Action: "second"})
	}()
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second refresh passed the credential session gate")
	case <-time.After(50 * time.Millisecond):
		close(releaseFirst)
	}
	for index := 0; index < 2; index++ {
		select {
		case response := <-responses:
			if response.State != "refreshed" {
				t.Fatalf("refresh response = %#v", response)
			}
		case <-time.After(time.Second):
			t.Fatal("serialized refresh did not complete")
		}
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second refresh never entered after the first completed")
	}
}

func TestCredentialSessionRefreshNotRequiredForNonGPGAgentBundles(t *testing.T) {
	tests := []struct {
		name       string
		bundle     CredentialBundleConfig
		peerSocket string
		wantCalls  int
	}{
		{name: "pivb-only"},
		{name: "standalone-ssh", peerSocket: "/standalone/agent.sock", wantCalls: 1},
	}
	tests[0].bundle.PIVB.Enable = true
	tests[1].bundle.SSHAgent.Enable = true
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
				if strings.Join(args, " ") == "--list-dirs agent-ssh-socket" {
					return "/provider/S.gpg-agent.ssh\n", "", nil
				}
				return "", "", errors.New("unexpected command")
			}}
			d := testCredentialSessionDaemon(runner, test.bundle)
			ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{
				"SSH_AUTH_SOCK": test.peerSocket,
			}})
			response := d.refreshCredentialSession(ctx, credentialSessionRefreshRequest{Bundle: "work"})
			if response.State != "not-required" || response.Required {
				t.Fatalf("refresh response = %#v", response)
			}
			if calls := runner.Calls(); len(calls) != test.wantCalls {
				t.Fatalf("calls = %#v, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestCredentialSessionHeadlessRequirementUsesProviderKeys(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.SSHAgent.Enable = true
	bundle.OpenPGP.SigningKeys = []string{"1111222233334444555566667777888899990000"}
	runner := &credentialSessionRunner{}
	d := testCredentialSessionDaemon(runner, bundle)
	d.config.Headless = true
	d.config.Credentials.GnuPG.ConfigureAgent = false

	response := d.refreshCredentialSession(context.Background(), credentialSessionRefreshRequest{Bundle: "work"})
	if response.State != "headless" || !response.Required {
		t.Fatalf("headless refresh response = %#v", response)
	}
	environment, pinentry := d.credentialSessionDoctorChecks(context.Background())
	if !environment.OK || pinentry.OK || !strings.Contains(pinentry.Detail, "headless node") {
		t.Fatalf("headless checks = %#v %#v", environment, pinentry)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("headless path spawned commands: %#v", calls)
	}
}

func TestGraphicalPinentryRequirementIncludesActiveLocalOpenPGPClaim(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	d := testCredentialSessionDaemon(&credentialSessionRunner{}, bundle)
	d.state = StateData{
		Node: Host{ID: "local-node"},
		Workspaces: map[string]*Workspace{
			"workspace": {
				ID: "workspace",
				CredentialClaim: &CredentialClaim{
					OwnerNodeID: "local-node", ProviderSource: "local", Bundle: "work",
				},
			},
		},
	}
	required, err := d.graphicalPinentryRequired(context.Background())
	if err != nil || !required {
		t.Fatalf("active local claim requirement = %v, %v", required, err)
	}
}

func TestGraphicalPinentryRequirementAggregatesSSHBundleChecks(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	callCount := 0
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		if strings.Join(args, " ") != "--list-dirs agent-ssh-socket" {
			return "", "", errors.New("unexpected command")
		}
		callCount++
		if callCount == 1 {
			return "/unrelated/S.gpg-agent.ssh\n", "", nil
		}
		return "/provider/S.gpg-agent.ssh\n", "", nil
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	d.config.Credentials.Bundles["second"] = bundle
	d.sshAgent = sshAgentInfo{EffectiveSocket: "/provider/S.gpg-agent.ssh"}

	required, err := d.graphicalPinentryRequired(context.Background())
	if err != nil || !required {
		t.Fatalf("aggregated SSH requirement = %v, %v", required, err)
	}
	if calls := runner.Calls(); len(calls) != 2 {
		t.Fatalf("SSH bundle checks = %#v, want two", calls)
	}
}

func TestGraphicalCredentialSessionResolvesRelativeWaylandAgainstPeerRuntime(t *testing.T) {
	runtimeDir := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(runtimeDir, "wayland-9"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if err := probeGraphicalCredentialSession(map[string]string{
		"WAYLAND_DISPLAY": "wayland-9",
		"XDG_RUNTIME_DIR": runtimeDir,
	}); err != nil {
		t.Fatal(err)
	}
	if err := probeGraphicalCredentialSession(map[string]string{"WAYLAND_DISPLAY": "wayland-9"}); err == nil || !strings.Contains(err.Error(), "peer XDG_RUNTIME_DIR") {
		t.Fatalf("missing runtime-dir error = %v", err)
	}
}

func TestLocalX11DisplayNumberRejectsRemoteHosts(t *testing.T) {
	for display, want := range map[string]string{
		":0":           "0",
		"unix:1.0":     "1",
		"localhost:10": "10",
	} {
		if got, ok := localX11DisplayNumber(display); !ok || got != want {
			t.Fatalf("local display %q = %q, %v", display, got, ok)
		}
	}
	for _, display := range []string{"evil.example.com:0", "tcp/evil.example.com:0", "192.0.2.1:0", "unix.example:0", ":-1"} {
		if got, ok := localX11DisplayNumber(display); ok {
			t.Fatalf("remote display %q accepted as %q", display, got)
		}
	}
	selected := selectCredentialSessionValues(map[string]string{
		"DISPLAY": "evil.example.com:0", "WAYLAND_DISPLAY": "wayland-1", "XDG_RUNTIME_DIR": "/run/user/1000",
	}, []string{"DISPLAY", "WAYLAND_DISPLAY"})
	if selected["DISPLAY"] != "" || selected["WAYLAND_DISPLAY"] != "wayland-1" {
		t.Fatalf("selected environment retained a remote display: %#v", selected)
	}
}

func TestAssuanEnvironmentParsersPreserveRawNULRecordBoundaries(t *testing.T) {
	names, err := parseAssuanDataNames("D GPG_TTY\x00\nD TERM\x00\nD WAYLAND_DISPLAY\x00\nOK\n")
	if err != nil || strings.Join(names, ",") != "GPG_TTY,TERM,WAYLAND_DISPLAY" {
		t.Fatalf("names = %#v, %v", names, err)
	}
	environment, err := parseAssuanDataEnvironment("D DISPLAY=:0\x00\nD DBUS_SESSION_BUS_ADDRESS=unix%3apath=/run/user/1000/bus\x00\nOK\n")
	if err != nil || environment["DISPLAY"] != ":0" || environment["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/run/user/1000/bus" {
		t.Fatalf("environment = %#v, %v", environment, err)
	}
}

func TestProviderEnvironmentRunnerComposesConfiguredOptions(t *testing.T) {
	underlying := &credentialSessionRunner{}
	runner := environmentCommandRunner{runner: underlying, environment: []string{"PROVIDER=only"}}
	_, _, err := runCommandWithOptions(context.Background(), runner, "provider-command", []string{"arg"}, commandOptions{
		Environment: []string{"CALLER=must-not-survive"}, Directory: "/provider", NewSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := underlying.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	call := calls[0]
	if strings.Join(call.Options.Environment, ",") != "PROVIDER=only" || call.Options.Directory != "/provider" || !call.Options.NewSession {
		t.Fatalf("composed options = %#v", call.Options)
	}
}

func TestProviderEnvironmentSanitizesManagedAndTerminalState(t *testing.T) {
	paths := Paths{StateDir: "/state/zka", AgentDir: "/run/user/1000/zka/agents"}
	environment, issues := sanitizeProviderEnvironment(paths, []string{
		"PATH=/usr/bin",
		"DISPLAY=:1",
		"LC_CTYPE=en_AU.UTF-8",
		"GPG_TTY=/dev/pts/4",
		"TERM=xterm-kitty",
		"INSIDE_EMACS=29.4,comint",
		"ZKA_WORKSPACE_ID=workspace",
		"ZKA_PANE_ID=pane",
		"ZKA_CREDENTIAL_ENVIRONMENT_VERSION=4",
		"GNUPGHOME=/state/zka/credentials/workspace/gnupg",
		"SSH_AUTH_SOCK=/run/user/1000/zka/agents/workspace.sock",
	})
	values := environmentMap(environment)
	for _, name := range []string{"GPG_TTY", "TERM", "INSIDE_EMACS", "ZKA_WORKSPACE_ID", "ZKA_PANE_ID", "ZKA_CREDENTIAL_ENVIRONMENT_VERSION", "GNUPGHOME", "SSH_AUTH_SOCK"} {
		if _, present := values[name]; present {
			t.Fatalf("%s survived provider sanitation: %#v", name, values)
		}
	}
	if values["DISPLAY"] != ":1" || values["LC_CTYPE"] != "en_AU.UTF-8" || len(issues) < 5 {
		t.Fatalf("sanitized environment=%#v issues=%#v", values, issues)
	}
}

func TestCredentialSessionSelectionUsesLivePeerThenDaemonFallback(t *testing.T) {
	d := &Daemon{providerEnvironment: []string{"DISPLAY=:2"}}
	standardNames := []string{"DISPLAY"}
	d.credentialSessionProbe = func(environment map[string]string) error {
		if environment["DISPLAY"] == ":1" {
			return nil
		}
		return errors.New("stale display")
	}
	selected, source, err := d.selectGraphicalCredentialSession(map[string]string{"DISPLAY": ":1"}, standardNames)
	if err != nil || source != "local CLI session" || selected["DISPLAY"] != ":1" {
		t.Fatalf("peer selection = %#v %q %v", selected, source, err)
	}

	d.credentialSessionProbe = func(environment map[string]string) error {
		if environment["DISPLAY"] == ":2" {
			return nil
		}
		return errors.New("stale display")
	}
	selected, source, err = d.selectGraphicalCredentialSession(map[string]string{"DISPLAY": ":3"}, standardNames)
	if err != nil || source != "zkad startup session" || selected["DISPLAY"] != ":2" {
		t.Fatalf("daemon fallback = %#v %q %v", selected, source, err)
	}
}

func TestCredentialSessionDoctorTreatsDifferentLiveCallerAsWarning(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.OpenPGP.SigningKeys = []string{"1111222233334444555566667777888899990000"}
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		switch args[0] {
		case "GETINFO std_env_names":
			return "D DISPLAY\nD DBUS_SESSION_BUS_ADDRESS\nOK\n", "", nil
		case "GETINFO std_startup_env":
			return "D DISPLAY=:2\nD DBUS_SESSION_BUS_ADDRESS=unix%3apath=/missing\nOK\n", "", nil
		default:
			return "", "", errors.New("unexpected command")
		}
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	d.credentialSessionProbe = func(environment map[string]string) error {
		if environment["DISPLAY"] == "" {
			return errors.New("no display")
		}
		return nil
	}
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{
		"DISPLAY":                  ":1",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/missing",
	}})
	_, pinentry := d.credentialSessionDoctorChecks(ctx)
	if !pinentry.OK || !pinentry.Warning || !strings.Contains(pinentry.Detail, "differs from the current caller") {
		t.Fatalf("pinentry check = %#v", pinentry)
	}
}

func TestCredentialSessionDoctorWarnsBeforeRefreshAndRejectsRegainedTerminalRouting(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.OpenPGP.SigningKeys = []string{"1111222233334444555566667777888899990000"}
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		switch args[0] {
		case "GETINFO std_env_names":
			return "D DISPLAY\nD GPG_TTY\nOK\n", "", nil
		case "GETINFO std_startup_env":
			return "D DISPLAY=:2\nD GPG_TTY=/dev/pts/4\nOK\n", "", nil
		default:
			return "", "", errors.New("unexpected command")
		}
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	d.credentialSessionProbe = func(environment map[string]string) error {
		if environment["DISPLAY"] == "" {
			return errors.New("no display")
		}
		return nil
	}
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{"DISPLAY": ":1"}})
	_, pinentry := d.credentialSessionDoctorChecks(ctx)
	if !pinentry.OK || !pinentry.Warning || !strings.Contains(pinentry.Detail, "pre-refresh terminal routing through GPG_TTY") {
		t.Fatalf("pre-refresh pinentry check = %#v", pinentry)
	}
	d.credentialSessionOwner = credentialSessionOwner{Action: "claim", UpdatedAt: time.Now().UTC()}
	_, pinentry = d.credentialSessionDoctorChecks(ctx)
	if pinentry.OK || !strings.Contains(pinentry.Detail, "regained terminal routing through GPG_TTY") {
		t.Fatalf("post-refresh pinentry check = %#v", pinentry)
	}
}

func TestCredentialSessionDoctorNamesMissingPeerPIDFDSupport(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.OpenPGP.SigningKeys = []string{"1111222233334444555566667777888899990000"}
	d := testCredentialSessionDaemon(&credentialSessionRunner{}, bundle)
	ctx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Err: errPeerPIDFDUnavailable})
	environment, pinentry := d.credentialSessionDoctorChecks(ctx)
	if environment.OK || environment.Detail != errPeerPIDFDUnavailable.Error() {
		t.Fatalf("provider environment check = %#v", environment)
	}
	if !pinentry.OK || !pinentry.Warning {
		t.Fatalf("pinentry check = %#v", pinentry)
	}
}

func TestCredentialProviderDiagnosticsBoundsSmartCardLeaseWait(t *testing.T) {
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.OpenPGP.SigningKeys = []string{"1111222233334444555566667777888899990000"}
	runner := &credentialSessionRunner{handler: func(_ context.Context, _ string, args []string, _ commandOptions) (string, string, error) {
		if strings.Join(args, " ") == "--list-dirs agent-extra-socket" {
			return "/definitely/missing/agent-extra.sock\n", "", nil
		}
		return "", "", errors.New("unexpected command")
	}}
	d := testCredentialSessionDaemon(runner, bundle)
	d.config.Headless = true
	d.cardLease = newSmartCardLease()
	release, err := d.cardLease.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	diagnostics := d.credentialProviderDiagnostics(ctx)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("doctor card-lease wait took %v", elapsed)
	}
	if !diagnostics.OpenPGPKeys.OK || !diagnostics.OpenPGPKeys.Warning || !strings.Contains(diagnostics.OpenPGPKeys.Detail, "smart-card lease is busy") {
		t.Fatalf("OpenPGP key diagnostic = %#v", diagnostics.OpenPGPKeys)
	}
}

func TestCallerSSHSocketPrefersPeerDerivedValue(t *testing.T) {
	d := &Daemon{paths: Paths{AgentDir: "/managed/agents"}}
	peerCtx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{
		"SSH_AUTH_SOCK": "/peer/agent.sock",
	}})
	if got := d.callerSSHSocket(peerCtx, "/wire/agent.sock"); got != "/peer/agent.sock" {
		t.Fatalf("peer socket = %q", got)
	}
	managedCtx := context.WithValue(context.Background(), localPeerContextKey{}, localPeerEnvironment{Environment: map[string]string{
		"SSH_AUTH_SOCK": "/managed/agents/workspace.sock",
	}})
	if got := d.callerSSHSocket(managedCtx, "/wire/agent.sock"); got != "" {
		t.Fatalf("managed peer socket = %q", got)
	}
	if got := d.callerSSHSocket(context.Background(), "/wire/agent.sock"); got != "/wire/agent.sock" {
		t.Fatalf("legacy fallback socket = %q", got)
	}
}

func TestReadLocalPeerEnvironmentUsesStablePeerPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	pid, environment, err := readLocalPeerEnvironment(server)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() || environment["PATH"] == "" {
		t.Fatalf("peer pid=%d environment=%#v", pid, environment)
	}
}

func TestReadLocalPeerEnvironmentRejectsExitedPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exited-peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestLocalPeerExitHelper")
	cmd.Env = append(os.Environ(), "ZKA_TEST_EXIT_PEER_SOCKET="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLocalPeerEnvironment(server); !errors.Is(err, errLocalPeerExited) {
		t.Fatalf("exited peer error = %v", err)
	}
}

func TestLocalPeerExitHelper(t *testing.T) {
	path := os.Getenv("ZKA_TEST_EXIT_PEER_SOCKET")
	if path == "" {
		return
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

var _ configuredCommandRunner = (*credentialSessionRunner)(nil)
