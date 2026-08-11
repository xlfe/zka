package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestCredentialLoopbackSignsAndVerifiesGitCommit(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skipf("gpg is required for the credential loopback test: %v", err)
	}
	gpgconf, err := exec.LookPath("gpgconf")
	if err != nil {
		t.Skipf("gpgconf is required for the credential loopback test: %v", err)
	}
	gpgConnectAgent, err := exec.LookPath("gpg-connect-agent")
	if err != nil {
		t.Skipf("gpg-connect-agent is required for the credential loopback test: %v", err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is required for the credential loopback test: %v", err)
	}

	// Assuan uses Unix sockets with a small platform path limit, so this test
	// needs testRoot rather than t.TempDir.
	root := testRoot(t)
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The production daemon is a systemd user service and always has a private
	// runtime directory. Model that explicitly so gpgconf does not fall back to
	// GNUPGHOME in hermetic builders which have no /run/user entry.
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	providerHome := filepath.Join(root, "provider-gnupg")
	if err := os.MkdirAll(providerHome, 0o700); err != nil {
		t.Fatal(err)
	}
	providerEnv := append(os.Environ(), "GNUPGHOME="+providerHome)
	runCredentialTestCommand(t, providerEnv, gpg, "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-gen-key", "zka loopback <zka@example.invalid>", "ed25519", "sign", "0")
	listing := runCredentialTestCommand(t, providerEnv, gpg, "--batch", "--with-colons", "--list-secret-keys")
	fingerprint := ""
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			fingerprint = fields[9]
			break
		}
	}
	if _, fingerprintErr := canonicalOpenPGPFingerprint(fingerprint); fingerprintErr != nil {
		t.Fatalf("generated key fingerprint %q: %v", fingerprint, fingerprintErr)
	}
	runCredentialTestCommand(t, providerEnv, gpgconf, "--homedir", providerHome, "--launch", "gpg-agent")
	t.Cleanup(func() {
		cmd := exec.Command(gpgconf, "--homedir", providerHome, "--kill", "gpg-agent")
		cmd.Env = providerEnv
		_ = cmd.Run()
	})

	cfg := defaultConfig()
	cfg.Credentials.GnuPG.Command = gpg
	cfg.Credentials.GnuPG.GPGConfCommand = gpgconf
	cfg.Credentials.GnuPG.GPGConnectAgentCommand = gpgConnectAgent
	cfg.Credentials.GnuPG.OperationTimeout = "5s"
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	bundle.OpenPGP.SigningKeys = []string{fingerprint}
	cfg.Credentials.DefaultBundle = "work"
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "zka-config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_CONFIG", configPath)
	t.Setenv("GNUPGHOME", providerHome)

	origin, err := NewDaemon(testPaths(filepath.Join(root, "origin")), ExecRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, origin)
	provider, err := NewDaemon(testPaths(filepath.Join(root, "provider")), ExecRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	provider.credentialInteractive = func() bool { return true }
	provider.desktop = &fakeNotifier{}

	workspace := createTestWorkspace(t, origin, 1)
	pane := firstPane(workspace)
	providerNode := provider.state.Node
	attachment, err := origin.registerAttachment(workspace.ID, Attachment{
		ID: "provider-attachment", Node: providerNode, Transport: Transport{Kind: "ssh"}, Endpoint: "ssh:provider:attachment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := origin.updateAttachment(attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Status: AttachmentReady,
		Views: map[string]RuntimeView{pane.ID: {PaneID: pane.ID, WindowID: 1, Ready: true}},
	}); err != nil {
		t.Fatal(err)
	}

	providerWire, originWire := net.Pipe()
	providerSession, err := yamux.Client(providerWire, remoteYamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	originSession, err := yamux.Server(originWire, remoteYamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = providerSession.Close(); _ = originSession.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	targets, err := newCredentialTargetSession(ctx, origin.paths, originSession, providerNode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(targets.close)
	go func() {
		for {
			stream, acceptErr := providerSession.AcceptStream()
			if acceptErr != nil {
				return
			}
			go func() {
				defer stream.Close()
				provider.handleCredentialStream(ctx, "origin", stream)
			}()
		}
	}()

	manifest, err := buildCredentialBundleManifest(ctx, provider.config, "work", ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	ownerAttachment := readyCredentialAttachment(t, origin, workspace, "provider-owner", providerNode.ID)
	if _, err := origin.claimWorkspaceCredentials(ctx, workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: providerNode, ProviderSource: "remote", OwnerAttachmentID: ownerAttachment.ID, Manifest: manifest,
	}); err != nil {
		t.Fatal(err)
	}
	// Reconciliation may replay preparation after any control reconnect. The
	// same manifest must therefore be harmless when prepared again.
	if _, err := origin.claimWorkspaceCredentials(ctx, workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: providerNode, ProviderSource: "remote", OwnerAttachmentID: ownerAttachment.ID, Manifest: manifest,
	}); err != nil {
		t.Fatalf("idempotent claim preparation: %v", err)
	}
	claimed, err := origin.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.cacheRemoteWorkspace("origin", claimed); err != nil {
		t.Fatal(err)
	}

	targetHome, err := credentialOpenPGPHome(origin.paths, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetSocket := strings.TrimSpace(runCredentialTestCommand(t, providerEnv, gpgconf, "--homedir", targetHome, "--list-dirs", "agent-socket"))
	waitForCredentialSocket(t, targetSocket)
	targetSocketInfo, err := os.Lstat(targetSocket)
	if err != nil {
		t.Fatal(err)
	}

	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(), "GNUPGHOME="+targetHome, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "init", "-q")
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "config", "user.name", "zka loopback")
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "config", "user.email", "zka@example.invalid")
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "config", "user.signingkey", fingerprint)
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "config", "gpg.program", gpg)
	if err := os.WriteFile(filepath.Join(repository, "payload.txt"), []byte("credential loopback\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "add", "payload.txt")
	runCredentialTestCommand(t, gitEnv, git, "-C", repository, "commit", "-q", "-S", "-m", "signed through zka")
	verification := runCredentialTestCommand(t, gitEnv, git, "-C", repository, "verify-commit", "HEAD")
	if !strings.Contains(strings.ToUpper(verification), strings.ToUpper(fingerprint)) {
		t.Fatalf("verify-commit did not report fingerprint %s:\n%s", fingerprint, verification)
	}

	conn, err := net.DialTimeout("unix", targetSocket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, assuanLineMax)
	if greeting, err := readAssuanLine(reader); err != nil || !strings.HasPrefix(greeting, "OK") {
		t.Fatalf("agent greeting = %q, %v", greeting, err)
	}
	if err := writeAssuanLine(conn, "SIGKEY "+testDeniedGrip); err != nil {
		t.Fatal(err)
	}
	denied, err := readAssuanLine(reader)
	if err != nil || !strings.HasPrefix(denied, "ERR 67108963") {
		t.Fatalf("non-allowlisted keygrip response = %q, %v", denied, err)
	}
	_ = conn.Close()
	targets.close()
	currentSocketInfo, err := os.Lstat(targetSocket)
	if err != nil {
		t.Fatalf("stable credential socket disappeared after transport close: %v", err)
	}
	if !os.SameFile(targetSocketInfo, currentSocketInfo) {
		t.Fatal("transport close replaced the stable credential socket")
	}
	unavailable, err := net.DialTimeout("unix", targetSocket, time.Second)
	if err != nil {
		t.Fatalf("dial stable credential socket after transport close: %v", err)
	}
	defer unavailable.Close()
	_ = unavailable.SetReadDeadline(time.Now().Add(time.Second))
	if greeting, err := readAssuanLine(bufio.NewReaderSize(unavailable, assuanLineMax)); err == nil {
		t.Fatalf("credential route remained usable after transport close: %q", greeting)
	}
}

func runCredentialTestCommand(t *testing.T, environment []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = environment
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func waitForCredentialSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("credential socket %s was not created", path)
}
