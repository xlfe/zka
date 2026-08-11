package zka

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Warning bool   `json:"warning,omitempty"`
	Detail  string `json:"detail"`
}

func runDoctor(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	origin := fs.String("origin", "", "test an origin through its SSH alias")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("doctor accepts no positional arguments")
	}
	cfg, cfgErr := LoadConfig()
	checks := []doctorCheck{{Name: "config", OK: cfgErr == nil, Detail: doctorDetail(cfgErr, envOr("ZKA_CONFIG", "built-in defaults"))}}
	if cfgErr != nil {
		return writeDoctorResult(checks, *jsonOut, stdout)
	}
	checks = append(checks, credentialsConfigDoctorCheck(cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	node, err := api.Node(ctx)
	daemonDetail := paths.Socket
	if err == nil {
		daemonDetail += "; node=" + node.ID
	}
	checks = append(checks, doctorCheck{Name: "daemon", OK: err == nil, Detail: doctorDetail(err, daemonDetail)})
	checks = append(checks, hookRelayDoctorCheck(paths))
	stateErr := NewStore(paths).Ensure()
	checks = append(checks, doctorCheck{Name: "state-dir", OK: stateErr == nil, Detail: doctorDetail(stateErr, paths.StateDir)})
	currentPaneCredentials, providerChecksUnsafe := currentPaneCredentialEnvironmentDoctorCheck(paths, cfg)
	checks = append(checks, currentPaneCredentials)
	commands := []struct{ name, command string }{
		{"kitty", cfg.Kitty.Command}, {"kitten", cfg.Kitty.KittenCommand},
		{"zmx", cfg.ZMX.Command}, {"ssh", cfg.SSH.Command},
		{"ntfy-send", cfg.Notifications.NtfyCommand},
		// Meaningful only because the config now carries an absolute store path:
		// a bare name resolves differently from a login shell and from zkad's
		// systemd unit, which is exactly how this one stayed broken unnoticed.
		{"swaymsg", cfg.Focus.SwayCommand},
	}
	if cfg.Integrations.CodexManagedHooks {
		commands = append(commands, struct{ name, command string }{"codex", "codex"})
	}
	if cfg.Integrations.ClaudeManagedHooks {
		commands = append(commands, struct{ name, command string }{"claude", "claude"})
	}
	if configHasOpenPGPBundle(cfg) {
		commands = append(commands,
			struct{ name, command string }{"gpg", cfg.Credentials.GnuPG.Command},
			struct{ name, command string }{"gpgconf", cfg.Credentials.GnuPG.GPGConfCommand},
			struct{ name, command string }{"gpg-connect-agent", cfg.Credentials.GnuPG.GPGConnectAgentCommand},
		)
	}
	if configHasPIVBBundle(cfg) {
		commands = append(commands, struct{ name, command string }{"pivb", cfg.Credentials.PIVB.Command})
		capabilityErr := ensureManagedPIVBCapability(ctx, cfg, ExecRunner{})
		checks = append(checks, doctorCheck{
			Name: "pivb-attachment-protocol", OK: capabilityErr == nil,
			Detail: doctorDetail(capabilityErr, fmt.Sprintf("%s supports attachment protocol %d", cfg.Credentials.PIVB.Command, managedPIVBAttachmentProtocol(cfg))),
		})
		pathCapabilityErr := ensurePaneVisiblePIVBCapability(ctx, cfg)
		checks = append(checks, doctorCheck{
			Name: "pivb-pane-capabilities", OK: pathCapabilityErr == nil,
			Detail: doctorDetail(pathCapabilityErr, fmt.Sprintf("PATH pivb supports attachment protocol %d", managedPIVBAttachmentProtocol(cfg))),
		})
	}
	// The view layer is skipped by configuration, not probing: a probe cannot
	// distinguish "no kitty because headless" from "no kitty because broken".
	// zmx, ssh, ntfy-send, and the agents stay checked — a headless origin is
	// exactly where they matter most, and ntfy is its only user-reaching
	// channel.
	headlessSkipped := map[string]bool{"kitty": true, "kitten": true, "swaymsg": true}
	for _, item := range commands {
		if cfg.Headless && headlessSkipped[item.name] {
			checks = append(checks, doctorCheck{Name: item.name, OK: true, Detail: "skipped on a headless origin"})
			continue
		}
		path, lookupErr := exec.LookPath(item.command)
		checks = append(checks, doctorCheck{Name: item.name, OK: lookupErr == nil, Detail: doctorDetail(lookupErr, path)})
	}
	checks = append(checks, swayIPCDoctorCheck(ctx, cfg.Headless, cfg.Notifications.DesktopEnabled, api.SwayIPC))
	if cfg.Headless {
		checks = append(checks, doctorCheck{Name: "kitty-watcher", OK: true, Detail: "skipped on a headless origin"})
	} else {
		watcherExists, watcherErr := configExists(cfg.Kitty.Watcher)
		if watcherErr == nil && !watcherExists {
			watcherErr = fmt.Errorf("not found")
		}
		checks = append(checks, doctorCheck{Name: "kitty-watcher", OK: watcherErr == nil, Detail: doctorDetail(watcherErr, cfg.Kitty.Watcher)})
	}
	checks = append(checks,
		managedHookDoctorCheck("codex-hooks", "/etc/codex/requirements.toml", "hook codex", cfg.Integrations.CodexManagedHooks),
		managedHookDoctorCheck("claude-hooks", "/etc/claude-code/managed-settings.d/50-zka.json", "hook claude", cfg.Integrations.ClaudeManagedHooks),
	)
	diagnostics, diagnosticsErr := api.CredentialProviderDiagnostics(ctx)
	daemonSSHAgent := diagnostics.SSHAgent
	var daemonSSHAgentErr error
	if diagnosticsErr == nil {
		checks = append(checks,
			diagnostics.ProviderEnvironment,
			diagnostics.CredentialsProvider,
			diagnostics.OpenPGPKeys,
			diagnostics.PinentrySession,
		)
	} else {
		daemonSSHAgent, daemonSSHAgentErr = api.SSHAgent(ctx)
		legacyDaemon := isUnknownDaemonOperation(diagnosticsErr, "credential_provider_diagnostics")
		detail := "daemon-side provider diagnostics failed: " + diagnosticsErr.Error()
		if legacyDaemon {
			detail = "zkad does not support daemon-side provider diagnostics; upgrade and restart the local daemon"
		}
		checks = append(checks,
			doctorCheck{Name: "provider-environment", OK: legacyDaemon, Warning: legacyDaemon, Detail: detail},
		)
		if providerChecksUnsafe {
			const unsafeDetail = "skipped: the current pane has an outdated managed credential environment; recreate it before testing provider credentials"
			checks = append(checks,
				doctorCheck{Name: "credentials-provider", OK: true, Detail: unsafeDetail},
				doctorCheck{Name: "openpgp-keys", OK: true, Detail: unsafeDetail},
			)
		} else {
			checks = append(checks, credentialsProviderDoctorCheck(ctx, cfg, ExecRunner{}), openPGPKeysDoctorCheck(ctx, cfg, ExecRunner{}))
		}
		checks = append(checks, doctorCheck{Name: "pinentry-session", OK: legacyDaemon, Warning: legacyDaemon, Detail: detail})
	}
	if configHasSSHAgentBundle(cfg) {
		sshAddCommand := filepath.Join(filepath.Dir(cfg.SSH.Command), "ssh-add")
		inspect := func(ctx context.Context, socket string) ([]string, error) {
			return inspectSSHAgent(ctx, sshAddCommand, socket)
		}
		if daemonSSHAgentErr != nil {
			checks = append(checks, doctorSSHAgentChecksWithoutDaemon(ctx, daemonSSHAgentErr, os.Getenv("SSH_AUTH_SOCK"), inspect)...)
		} else {
			checks = append(checks, doctorSSHAgentChecks(ctx, daemonSSHAgent, os.Getenv("SSH_AUTH_SOCK"), inspect)...)
		}
	}
	var credentialStatus credentialStatusResponse
	var credentialStatusErr error
	if *origin != "" {
		var workspaces []*Workspace
		remoteErr := api.RemoteCall(ctx, *origin, "list", nil, &workspaces)
		detail := fmt.Sprintf("%s (%d workspaces)", *origin, len(workspaces))
		checks = append(checks, doctorCheck{Name: "remote-control", OK: remoteErr == nil, Detail: doctorDetail(remoteErr, detail)})
		credentialStatusErr = api.RemoteCall(ctx, *origin, "credentials_status", nil, &credentialStatus)
		localStatus, localStatusErr := api.CredentialStatus(ctx, "")
		if localStatusErr == nil {
			credentialStatus.Transport = localStatus.Transport
		} else if credentialStatusErr == nil {
			credentialStatusErr = localStatusErr
		}
	} else {
		credentialStatus, credentialStatusErr = api.CredentialStatus(ctx, "")
	}
	checks = append(checks,
		credentialsClaimDoctorCheck(credentialStatus, credentialStatusErr),
		credentialsTransportDoctorCheck(credentialStatus.Transport, credentialStatusErr),
		credentialEnvironmentInventoryDoctorCheck(credentialStatus, credentialStatusErr),
	)
	// One round trip shared by both checks that need the workspace set.
	workspaces, workspacesErr := api.Workspaces(ctx)
	checks = append(checks,
		notificationDeliveryCheck(workspaces, workspacesErr, time.Now().UTC()),
		desktopNotificationCheck(ctx, cfg.Notifications.DesktopEnabled, probeDesktopNotifier),
		topologyRenderableCheck(workspaces, workspacesErr),
	)
	return writeDoctorResult(checks, *jsonOut, stdout)
}

func hookRelayDoctorCheck(paths Paths) doctorCheck {
	const name = "hook-relays"
	root := hookRelayRoot(paths)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return doctorCheck{Name: name, OK: true, Detail: "no active hook relays"}
	}
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || !ok || rootStat.Uid != uint32(os.Getuid()) || rootInfo.Mode().Perm() != 0o700 {
		return doctorCheck{Name: name, Detail: fmt.Sprintf("%s is not an owned 0700 directory", root)}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	active, starting, stale := 0, 0, 0
	var problems []string
	for _, entry := range entries {
		if !entry.IsDir() || !validHookRelaySessionID(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		dirInfo, statErr := os.Lstat(dir)
		if statErr != nil {
			problems = append(problems, entry.Name()+": "+statErr.Error())
			continue
		}
		dirStat, owned := dirInfo.Sys().(*syscall.Stat_t)
		if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || !owned || dirStat.Uid != uint32(os.Getuid()) || dirInfo.Mode().Perm() != 0o700 {
			problems = append(problems, entry.Name()+": session is not an owned 0700 directory")
			continue
		}
		young := time.Since(dirInfo.ModTime()) < hookRelayStaleGrace
		lock, lockErr := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_RDONLY, 0)
		if lockErr != nil {
			if young && errors.Is(lockErr, os.ErrNotExist) {
				starting++
				continue
			}
			problems = append(problems, entry.Name()+": "+lockErr.Error())
			continue
		}
		lockErr = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		isActive := errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN)
		if lockErr == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		_ = lock.Close()
		if lockErr != nil && !isActive {
			problems = append(problems, entry.Name()+": inspect session lock: "+lockErr.Error())
			continue
		}
		if lockErr == nil && young {
			starting++
			continue
		}
		socket := agentRelaySocketPath(root, entry.Name())
		socketInfo, socketErr := os.Lstat(socket)
		if young && errors.Is(socketErr, os.ErrNotExist) {
			starting++
			continue
		}
		if socketErr != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 {
			detail := "socket is not a 0600 Unix socket"
			if socketErr != nil {
				detail = socketErr.Error()
			}
			problems = append(problems, entry.Name()+": "+detail)
			continue
		}
		if isActive {
			active++
		} else {
			stale++
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return doctorCheck{Name: name, Detail: strings.Join(problems, "; ")}
	}
	detail := fmt.Sprintf("%d active", active)
	if starting != 0 {
		detail += fmt.Sprintf(", %d starting", starting)
	}
	if stale != 0 {
		detail += fmt.Sprintf(", %d stale (removed on next relay startup)", stale)
	}
	return doctorCheck{Name: name, OK: true, Warning: stale != 0, Detail: detail}
}

func currentPaneCredentialEnvironmentDoctorCheck(paths Paths, configs ...Config) (doctorCheck, bool) {
	cfg := defaultConfig()
	if len(configs) != 0 {
		cfg = configs[0]
	}
	const name = "current-pane-credentials"
	paneID := os.Getenv("ZKA_PANE_ID")
	if paneID == "" {
		return doctorCheck{Name: name, OK: true, Detail: "not running inside a zka pane"}, false
	}
	workspaceID := os.Getenv("ZKA_WORKSPACE_ID")
	rawVersion := os.Getenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION")
	if rawVersion == "" || rawVersion == "0" {
		detail := fmt.Sprintf("pane %s uses version 0 direct credentials and requires explicit backend recreation", shortID(paneID))
		if home, homeErr := credentialOpenPGPHome(paths, workspaceID); homeErr == nil {
			detail += fmt.Sprintf("; managed SSH_AUTH_SOCK=%s; managed GNUPGHOME=%s", agentRelaySocketPath(paths.AgentDir, workspaceID), home)
		}
		return doctorCheck{Name: name, Detail: detail}, true
	}
	version, err := strconv.Atoi(rawVersion)
	if err != nil || version < 0 {
		return doctorCheck{Name: name, Detail: fmt.Sprintf("pane %s has invalid credential environment version %q", shortID(paneID), rawVersion)}, true
	}
	requiredVersion := credentialEnvironmentVersionForConfig(cfg)
	if version == requiredVersion {
		return doctorCheck{Name: name, OK: true, Detail: fmt.Sprintf("pane %s uses managed credential environment v%d", shortID(paneID), version)}, false
	}
	if version > credentialEnvironmentVersion {
		return doctorCheck{Name: name, Detail: fmt.Sprintf("pane %s uses credential environment v%d, newer than this zka supports", shortID(paneID), version)}, true
	}

	detail := fmt.Sprintf("pane %s uses credential environment v%d but routing mode %s requires v%d; run `zka workspace reconcile --recreate-backends %s`", shortID(paneID), version, cfg.Credentials.PIVB.RoutingMode, requiredVersion, workspaceID)
	if home, homeErr := credentialOpenPGPHome(paths, workspaceID); homeErr == nil {
		// These are origin-side paths derived solely from the workspace ID. Do
		// not include the provider's resolved SSH or gpg-agent socket paths:
		// those are deliberately memory-only.
		detail += fmt.Sprintf("; origin SSH_AUTH_SOCK=%s; origin GNUPGHOME=%s", agentRelaySocketPath(paths.AgentDir, workspaceID), home)
	}
	return doctorCheck{Name: name, Detail: detail}, true
}

func configHasOpenPGPBundle(cfg Config) bool {
	for _, bundle := range cfg.Credentials.Bundles {
		if bundle.OpenPGP.Enable {
			return true
		}
	}
	return false
}

func configHasSSHAgentBundle(cfg Config) bool {
	for _, bundle := range cfg.Credentials.Bundles {
		if bundle.SSHAgent.Enable {
			return true
		}
	}
	return false
}

func configHasOpenPGPProviderKeys(cfg Config) bool {
	for _, bundle := range cfg.Credentials.Bundles {
		if bundle.OpenPGP.Enable && len(bundle.OpenPGP.SigningKeys) != 0 {
			return true
		}
	}
	return false
}

func credentialsConfigDoctorCheck(cfg Config) doctorCheck {
	if len(cfg.Credentials.Bundles) == 0 {
		return doctorCheck{Name: "credentials-config", OK: true, Detail: "no credential bundles configured"}
	}
	names := make([]string, 0, len(cfg.Credentials.Bundles))
	for name, bundle := range cfg.Credentials.Bundles {
		capabilities := make([]string, 0, 3)
		if bundle.SSHAgent.Enable {
			capabilities = append(capabilities, credentialCapabilitySSH)
		}
		if bundle.OpenPGP.Enable {
			capabilities = append(capabilities, credentialCapabilityOpenPGP)
		}
		if bundle.PIVB.Enable {
			capabilities = append(capabilities, credentialCapabilityPIVB)
		}
		names = append(names, name+"="+strings.Join(capabilities, "+"))
	}
	sort.Strings(names)
	detail := strings.Join(names, ", ")
	if cfg.Credentials.DefaultBundle != "" {
		detail += "; default=" + cfg.Credentials.DefaultBundle
	}
	return doctorCheck{Name: "credentials-config", OK: true, Detail: detail}
}

func credentialsProviderDoctorCheck(ctx context.Context, cfg Config, runner CommandRunner) doctorCheck {
	return credentialsProviderDoctorCheckWithAgent(ctx, cfg, runner, newSSHAgentInfo(cfg, os.Getenv("SSH_AUTH_SOCK")))
}

func credentialsProviderDoctorCheckWithAgent(ctx context.Context, cfg Config, runner CommandRunner, agent sshAgentInfo) doctorCheck {
	const name = "credentials-provider"
	if len(cfg.Credentials.Bundles) == 0 {
		return doctorCheck{Name: name, OK: true, Detail: "no provider capabilities configured"}
	}
	var problems []string
	sshRequired := false
	pivbRequired := false
	for _, bundle := range cfg.Credentials.Bundles {
		sshRequired = sshRequired || bundle.SSHAgent.Enable
		pivbRequired = pivbRequired || bundle.PIVB.Enable
	}
	if sshRequired {
		conn, err := dialAgentSocket(agent.EffectiveSocket)
		if err != nil {
			problems = append(problems, "ssh-agent: "+err.Error())
		} else {
			_ = conn.Close()
		}
	}
	if configHasOpenPGPProviderKeys(cfg) {
		socket, _, err := runner.Run(ctx, cfg.Credentials.GnuPG.GPGConfCommand, "--list-dirs", "agent-extra-socket")
		if err != nil {
			problems = append(problems, "openpgp: "+err.Error())
		} else {
			conn, dialErr := net.DialTimeout("unix", strings.TrimSpace(socket), 500*time.Millisecond)
			if dialErr != nil {
				problems = append(problems, "openpgp: "+dialErr.Error())
			} else {
				_ = conn.Close()
			}
		}
	}
	if pivbRequired {
		socket := credentialPIVBForwardSocket(cfg)
		if socket == "" {
			problems = append(problems, "pivb: forwarding socket is not configured")
		} else {
			conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
			if err != nil {
				problems = append(problems, "pivb: "+err.Error())
			} else {
				_ = conn.Close()
			}
		}
	}
	if len(problems) != 0 {
		return doctorCheck{Name: name, Detail: strings.Join(problems, "; ")}
	}
	return doctorCheck{Name: name, OK: true, Detail: "configured provider sockets are reachable"}
}

func openPGPKeysDoctorCheck(ctx context.Context, cfg Config, runner CommandRunner) doctorCheck {
	const name = "openpgp-keys"
	bundles := make([]string, 0, len(cfg.Credentials.Bundles))
	softwareBacked := false
	for bundleName, bundle := range cfg.Credentials.Bundles {
		if !bundle.OpenPGP.Enable || len(bundle.OpenPGP.SigningKeys) == 0 {
			continue
		}
		manifest, err := buildOpenPGPManifest(ctx, cfg, bundle.OpenPGP.SigningKeys, runner)
		if err != nil {
			return doctorCheck{Name: name, Detail: bundleName + ": " + err.Error()}
		}
		keys := make([]string, 0, len(manifest.AllowedKeygrips))
		for grip, fingerprint := range manifest.AllowedKeygrips {
			backing := "software"
			if manifest.CardBacked[grip] {
				backing = "card"
			} else {
				softwareBacked = true
			}
			keys = append(keys, shortFingerprint(fingerprint)+"/"+grip[:8]+":"+backing)
		}
		sort.Strings(keys)
		bundles = append(bundles, bundleName+"="+strings.Join(keys, ","))
	}
	if len(bundles) == 0 {
		return doctorCheck{Name: name, OK: true, Detail: "no OpenPGP provider keys configured on this node"}
	}
	sort.Strings(bundles)
	return doctorCheck{Name: name, OK: true, Warning: softwareBacked, Detail: strings.Join(bundles, "; ")}
}

func credentialsClaimDoctorCheck(status credentialStatusResponse, err error) doctorCheck {
	const name = "credentials-claim"
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	var claimed []string
	for _, workspace := range status.Workspaces {
		if workspace.State == "unclaimed" {
			continue
		}
		capabilities := make([]string, 0, len(workspace.Capabilities))
		for capability, view := range workspace.Capabilities {
			capabilities = append(capabilities, capability+":"+view.State)
		}
		sort.Strings(capabilities)
		claimed = append(claimed, fmt.Sprintf("%s=%s@%s[%s]", workspace.WorkspaceName, workspace.Bundle, shortID(workspace.OwnerNode), strings.Join(capabilities, ",")))
	}
	if len(claimed) == 0 {
		return doctorCheck{Name: name, OK: true, Detail: "no workspaces currently claim credentials"}
	}
	sort.Strings(claimed)
	return doctorCheck{Name: name, OK: true, Detail: strings.Join(claimed, "; ")}
}

func credentialEnvironmentInventoryDoctorCheck(status credentialStatusResponse, err error) doctorCheck {
	const name = "credential-environment"
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	var affected []string
	for _, workspace := range status.Workspaces {
		if len(workspace.RecreatePaneIDs) == 0 {
			continue
		}
		detail := fmt.Sprintf("%s=%s", workspace.WorkspaceName, strings.Join(workspace.RecreatePaneIDs, ","))
		if workspace.RecreationDetail != "" {
			detail += " (" + workspace.RecreationDetail + ")"
		}
		affected = append(affected, detail)
	}
	if len(affected) == 0 {
		return doctorCheck{Name: name, OK: true, Detail: "no panes require credential-environment recreation"}
	}
	sort.Strings(affected)
	return doctorCheck{Name: name, Detail: strings.Join(affected, "; ")}
}

func credentialsTransportDoctorCheck(status credentialTransportView, err error) doctorCheck {
	const name = "credentials-transport"
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	detail := status.State
	if status.Attempts != 0 {
		detail += fmt.Sprintf(" after %d attempts", status.Attempts)
	}
	if status.LastError != "" {
		detail += ": " + status.LastError
	}
	return doctorCheck{Name: name, OK: status.State == "ready" || status.State == "idle", Detail: detail}
}

// desktopNotifyProbe proves the desktop channel and names what answered.
type desktopNotifyProbe func(context.Context) (string, error)

// probeDesktopNotifier is the production probe: it posts and immediately
// withdraws a real notification through the same transport the daemon uses.
// Nothing ever proved that path before, which is how a transport that could
// never work survived for the life of the project.
func probeDesktopNotifier(ctx context.Context) (string, error) {
	notifier := newDBusNotifier(log.New(io.Discard, "", 0), nil)
	defer notifier.Shutdown()
	return notifier.Probe(ctx)
}

func desktopNotificationCheck(ctx context.Context, enabled bool, probe desktopNotifyProbe) doctorCheck {
	const name = "notification-desktop"
	if !enabled {
		return doctorCheck{Name: name, OK: true, Detail: "disabled in zka configuration"}
	}
	server, err := probe(ctx)
	if err != nil {
		// A machine with no session bus is a headless or remote origin, not a
		// broken one: the desktop channel only ever fires where there is a local
		// Kitty attachment anyway.
		if errors.Is(err, errNoSessionBus) {
			return doctorCheck{Name: name, OK: true, Detail: "no session bus to probe"}
		}
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	return doctorCheck{Name: name, OK: true, Detail: "delivered and withdrew a probe via " + server}
}

type swayIPCProbe func(context.Context) (swaySocketInfo, error)

func swayIPCDoctorCheck(ctx context.Context, headless, desktopEnabled bool, probe swayIPCProbe) doctorCheck {
	const name = "sway-ipc"
	if headless {
		return doctorCheck{Name: name, OK: true, Detail: "skipped on a headless origin"}
	}
	if !desktopEnabled {
		return doctorCheck{Name: name, OK: true, Detail: "disabled with desktop notifications"}
	}
	socket, err := probe(ctx)
	if err != nil {
		detail := err.Error()
		if strings.Contains(detail, "unknown operation") {
			detail += " (restart zkad after upgrading)"
		}
		return doctorCheck{Name: name, Detail: detail}
	}
	failedHints := make([]string, 0, 2)
	for _, attempt := range socket.FailedAttempts {
		if attempt.Source != "SWAYSOCK" && attempt.Source != "I3SOCK" {
			continue
		}
		failedHints = append(failedHints, fmt.Sprintf("%s=%s is stale (%s)", attempt.Source, attempt.Path, attempt.Error))
	}
	if len(failedHints) != 0 {
		return doctorCheck{
			Name: name, OK: true, Warning: true,
			Detail: fmt.Sprintf("recovered via %s at %s; %s; fix your Sway session environment import; zka recovery does not repair other programs in the session",
				socket.Source, socket.Path, strings.Join(failedHints, "; ")),
		}
	}
	return doctorCheck{
		Name:   name,
		OK:     true,
		Detail: socket.Path + " via " + socket.Source,
	}
}

// notificationDeliveryCheck turns the delivery ledger that only `zka workspace
// inspect` ever showed into a pass/fail signal. An abandoned record is a channel
// that spent its retry budget; a long-stale pending record is a reservation no
// worker ever owned, which is the signature of a shutdown that dropped it.
func notificationDeliveryCheck(workspaces []*Workspace, listErr error, now time.Time) doctorCheck {
	const name = "notification-delivery"
	if listErr != nil {
		return doctorCheck{Name: name, Detail: listErr.Error()}
	}
	const stalePending = 5 * time.Minute
	sent, retrying := 0, 0
	var problems []string
	for _, workspace := range workspaces {
		if workspace == nil {
			continue
		}
		for _, pane := range workspace.Panes {
			if pane == nil {
				continue
			}
			for _, record := range pane.SortedNotifications() {
				switch notificationRecordStatus(record) {
				case "sent":
					sent++
				case "retrying":
					retrying++
				case "failed":
					problems = append(problems, fmt.Sprintf("%s: %s/%s abandoned after %d attempts (%s)",
						record.Channel, workspace.Name, shortID(pane.ID), record.Attempts, record.LastError))
				case "pending":
					if !record.LastTriedAt.IsZero() && now.Sub(record.LastTriedAt) >= stalePending {
						problems = append(problems, fmt.Sprintf("%s: %s/%s reserved but never attempted (%s ago)",
							record.Channel, workspace.Name, shortID(pane.ID),
							now.Sub(record.LastTriedAt).Round(time.Minute)))
					}
				}
			}
		}
	}
	if len(problems) != 0 {
		return doctorCheck{Name: name, Detail: strings.Join(problems, "; ")}
	}
	return doctorCheck{
		Name: name, OK: true,
		Detail: fmt.Sprintf("%d delivered, %d retrying, 0 failed", sent, retrying),
	}
}

// topologyRenderableCheck turns "the desired topology cannot be expressed as a
// Kitty session" from an outage into a diagnosable pre-flight condition. That
// state is what drove the reconciler to rebuild every window on a timer.
func topologyRenderableCheck(workspaces []*Workspace, err error) doctorCheck {
	if err != nil {
		return doctorCheck{Name: "topology-renderable", OK: false, Detail: err.Error()}
	}
	var problems []string
	checked := 0
	for _, workspace := range workspaces {
		if workspace == nil || len(workspace.Topology.Roots) == 0 {
			continue
		}
		checked++
		if digest := topologyStructuralDigest(workspace.Topology.Roots); digest != workspace.Topology.Digest {
			problems = append(problems, fmt.Sprintf("%s: stored digest does not describe stored topology", workspace.Name))
		}
		if _, renderErr := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, ""); renderErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", workspace.Name, renderErr))
		}
	}
	if len(problems) != 0 {
		return doctorCheck{Name: "topology-renderable", OK: false, Detail: strings.Join(problems, "; ")}
	}
	return doctorCheck{
		Name: "topology-renderable", OK: true,
		Detail: fmt.Sprintf("%d workspace topologies render to a Kitty session", checked),
	}
}

func managedHookDoctorCheck(name, path, command string, enabled bool) doctorCheck {
	if !enabled {
		return doctorCheck{Name: name, OK: true, Detail: "disabled in zka configuration"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	if !strings.Contains(string(b), command) {
		return doctorCheck{Name: name, Detail: "managed zka hook not found in " + path}
	}
	return doctorCheck{Name: name, OK: true, Detail: path}
}

type sshAgentInspector func(context.Context, string) ([]string, error)

func doctorSSHAgentChecks(ctx context.Context, daemonAgent sshAgentInfo, callerSocket string, inspect sshAgentInspector) []doctorCheck {
	daemonFingerprints, daemonErr := inspect(ctx, daemonAgent.EffectiveSocket)
	callerFingerprints, callerErr := daemonFingerprints, daemonErr
	if !sameSSHAgentSocket(daemonAgent.EffectiveSocket, callerSocket) {
		callerFingerprints, callerErr = inspect(ctx, callerSocket)
	}
	daemonDetail := sshAgentDetail(daemonAgent.EffectiveSocket, daemonFingerprints, daemonErr)
	if daemonAgent.IdentityAgent != "" {
		daemonDetail = fmt.Sprintf("configured %s; effective %s", daemonAgent.IdentityAgent, daemonDetail)
	} else if daemonAgent.InheritedSocket != "" {
		daemonDetail = "inherited " + daemonDetail
	}
	checks := []doctorCheck{
		{Name: "zkad-ssh-agent", OK: daemonErr == nil, Detail: daemonDetail},
		{Name: "caller-ssh-agent", OK: callerErr == nil, Detail: sshAgentDetail(callerSocket, callerFingerprints, callerErr)},
	}
	match := doctorCheck{Name: "ssh-agent-match"}
	switch {
	case daemonErr != nil || callerErr != nil:
		match.Detail = fmt.Sprintf("cannot compare agents: zkad uses %s; caller uses %s", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	case sameSSHAgentSocket(daemonAgent.EffectiveSocket, callerSocket):
		match.OK = true
		match.Detail = "zkad and caller use " + displaySSHAgentSocket(callerSocket)
	case equalStrings(daemonFingerprints, callerFingerprints):
		match.OK = true
		match.Detail = fmt.Sprintf("different sockets expose the same identities: zkad uses %s; caller uses %s", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	default:
		match.Detail = fmt.Sprintf("zkad uses %s; caller uses %s; agents expose different identities", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	}
	return append(checks, match)
}

func doctorSSHAgentChecksWithoutDaemon(ctx context.Context, daemonErr error, callerSocket string, inspect sshAgentInspector) []doctorCheck {
	callerFingerprints, callerErr := inspect(ctx, callerSocket)
	callerDetail := sshAgentDetail(callerSocket, callerFingerprints, callerErr)
	return []doctorCheck{
		{Name: "zkad-ssh-agent", Detail: "query zkad SSH agent: " + daemonErr.Error()},
		{Name: "caller-ssh-agent", OK: callerErr == nil, Detail: callerDetail},
		{Name: "ssh-agent-match", Detail: fmt.Sprintf("cannot compare agents: zkad SSH agent is unavailable; caller uses %s", displaySSHAgentSocket(callerSocket))},
	}
}

func inspectSSHAgent(ctx context.Context, sshAddCommand, socket string) ([]string, error) {
	if socket == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
	}
	if socket == "none" {
		return nil, fmt.Errorf("disabled by IdentityAgent=none")
	}
	info, err := os.Stat(socket)
	if err != nil {
		return nil, fmt.Errorf("inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("not a Unix socket")
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(inspectCtx, sshAddCommand, "-L")
	cmd.Env = sshAgentEnvironment(socket)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("list identities: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return sshPublicKeyFingerprints(string(output))
}

func sshPublicKeyFingerprints(output string) ([]string, error) {
	var fingerprints []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid public key from ssh-agent")
		}
		blob, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil {
			return nil, fmt.Errorf("decode public key from ssh-agent: %w", err)
		}
		digest := sha256.Sum256(blob)
		fingerprints = append(fingerprints, "SHA256:"+base64.RawStdEncoding.EncodeToString(digest[:]))
	}
	if len(fingerprints) == 0 {
		return nil, fmt.Errorf("agent contains no identities")
	}
	sort.Strings(fingerprints)
	return fingerprints, nil
}

func sshAgentEnvironment(socket string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SSH_AUTH_SOCK=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "SSH_AUTH_SOCK="+socket)
}

func siblingSSHCommand(sshCommand, name string) string {
	if strings.ContainsRune(sshCommand, filepath.Separator) {
		return filepath.Join(filepath.Dir(sshCommand), name)
	}
	return name
}

func sshAgentDetail(socket string, fingerprints []string, err error) string {
	detail := displaySSHAgentSocket(socket)
	if err != nil {
		return detail + ": " + err.Error()
	}
	return detail + " (" + strings.Join(fingerprints, ", ") + ")"
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func writeDoctorResult(checks []doctorCheck, jsonOut bool, stdout io.Writer) (int, error) {
	failed := false
	for _, check := range checks {
		failed = failed || !check.OK
	}
	if jsonOut {
		if err := writeJSON(stdout, checks); err != nil {
			return 1, err
		}
	} else {
		for _, check := range checks {
			status := "ok"
			if !check.OK {
				status = "FAIL"
			} else if check.Warning {
				status = "WARN"
			}
			fmt.Fprintf(stdout, "%-5s %-16s %s\n", status, check.Name, check.Detail)
		}
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

func doctorDetail(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
