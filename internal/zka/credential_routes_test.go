package zka

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreparePaneDoesNotReserveBackendWhenStableCredentialRouteIsUnavailable(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	pane := firstPane(workspace)
	blocker, err := listenUnix(agentRelaySocketPath(d.paths.AgentDir, workspace.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })

	if _, err := d.preparePane(workspacePaneRequest{Workspace: workspace.ID, Pane: pane.ID}); err == nil {
		t.Fatal("prepare pane succeeded while another listener owned the stable credential route")
	}
	d.mu.Lock()
	result := d.state.Workspaces[workspace.ID].Panes[pane.ID].Clone()
	d.mu.Unlock()
	if result.BackendStart || result.BackendCreated {
		t.Fatalf("failed route preflight reserved backend start: %#v", result)
	}
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].Panes[pane.ID].BackendCreated = true
	d.mu.Unlock()
	prepared, err := d.preparePane(workspacePaneRequest{Workspace: workspace.ID, Pane: pane.ID})
	if err != nil || prepared.Create {
		t.Fatalf("existing backend attach was blocked by route preflight: create=%t err=%v", prepared.Create, err)
	}
}

func TestPreparePaneBlocksRouteUnsafePIVBBackend(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	bundle := CredentialBundleConfig{}
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	stored := d.state.Workspaces[workspace.ID].Panes[pane.ID]
	stored.BackendCreated = true
	stored.CredentialEnvironmentVersion = legacyCredentialEnvironmentVersion
	d.mu.Unlock()

	_, err = d.preparePane(workspacePaneRequest{Workspace: workspace.ID, Pane: pane.ID})
	if err == nil || !strings.Contains(err.Error(), "--recreate-backends "+workspace.ID) {
		t.Fatalf("route-unsafe attach error = %v", err)
	}
}

func TestStableSSHRouteSwitchesAcrossLocalAndRemoteProviders(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)

	localAgentPath := filepath.Join(d.paths.RuntimeDir, "local-agent.sock")
	localAgent, err := listenUnix(localAgentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localAgent.Close() })
	go serveCredentialTestByte(localAgent, 'L')
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, localAgentPath, localOwner.ID, 0); err != nil {
		t.Fatal(err)
	}
	routePath := agentRelaySocketPath(d.paths.AgentDir, workspace.ID)
	d.credentialMu.Lock()
	stableRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilitySSH)]
	d.credentialMu.Unlock()
	if stableRoute == nil {
		t.Fatal("local activation did not publish stable SSH route")
	}
	stableRouteInfo, err := os.Lstat(routePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := credentialTestRoundTrip(t, routePath, 'a'); got != 'L' {
		t.Fatalf("local route reply = %q", got)
	}

	provider := Host{ID: "provider", Name: "laptop"}
	providerAttachment := readyCredentialAttachment(t, d, workspace, "provider-owner", provider.ID)
	brokerPath := filepath.Join(d.paths.RuntimeDir, "provider-broker.sock")
	broker, err := listenUnix(brokerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	hellos := make(chan credentialStreamHello, 1)
	go func() {
		for {
			conn, acceptErr := broker.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				hello, helloErr := readCredentialStreamHello(conn)
				if helloErr != nil {
					return
				}
				hellos <- hello
				var request [1]byte
				if _, readErr := io.ReadFull(conn, request[:]); readErr == nil {
					_, _ = conn.Write([]byte{'R'})
				}
			}()
		}
	}()
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: provider, State: "ready", Endpoint: brokerPath}); err != nil {
		t.Fatal(err)
	}
	status, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: provider, ProviderSource: "remote",
		OwnerAttachmentID: providerAttachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", SSH: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.credentialMu.Lock()
	transferredRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilitySSH)]
	d.credentialMu.Unlock()
	if transferredRoute != stableRoute {
		t.Fatal("provider transfer replaced the stable SSH listener")
	}
	if got := credentialTestRoundTrip(t, routePath, 'b'); got != 'R' {
		t.Fatalf("remote route reply = %q", got)
	}
	select {
	case hello := <-hellos:
		if hello.Workspace != workspace.ID || hello.Bundle != "work" || hello.Capability != credentialCapabilitySSH || hello.Generation != status.Generation {
			t.Fatalf("remote stream hello = %#v", hello)
		}
	case <-time.After(time.Second):
		t.Fatal("remote route did not reach the provider broker")
	}
	secondProvider := Host{ID: "second-provider", Name: "second-laptop"}
	secondProviderAttachment := readyCredentialAttachment(t, d, workspace, "second-provider-owner", secondProvider.ID)
	secondBrokerPath := filepath.Join(d.paths.RuntimeDir, "second-provider-broker.sock")
	secondBroker, err := listenUnix(secondBrokerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondBroker.Close() })
	secondHellos := make(chan credentialStreamHello, 1)
	go func() {
		for {
			conn, acceptErr := secondBroker.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				hello, helloErr := readCredentialStreamHello(conn)
				if helloErr != nil {
					return
				}
				secondHellos <- hello
				var request [1]byte
				if _, readErr := io.ReadFull(conn, request[:]); readErr == nil {
					_, _ = conn.Write([]byte{'S'})
				}
			}()
		}
	}()
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: secondProvider, State: "ready", Endpoint: secondBrokerPath}); err != nil {
		t.Fatal(err)
	}
	secondStatus, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: secondProvider, ProviderSource: "remote",
		OwnerAttachmentID: secondProviderAttachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", SSH: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondStatus.Generation <= status.Generation {
		t.Fatalf("second remote generation = %d, want > %d", secondStatus.Generation, status.Generation)
	}
	d.credentialMu.Lock()
	secondTransferredRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilitySSH)]
	d.credentialMu.Unlock()
	if secondTransferredRoute != stableRoute {
		t.Fatal("remote-to-remote transfer replaced the stable SSH listener")
	}
	if got := credentialTestRoundTrip(t, routePath, 'c'); got != 'S' {
		t.Fatalf("second remote route reply = %q", got)
	}
	select {
	case hello := <-secondHellos:
		if hello.Generation != secondStatus.Generation || hello.Workspace != workspace.ID {
			t.Fatalf("second remote stream hello = %#v", hello)
		}
	case <-time.After(time.Second):
		t.Fatal("remote-to-remote route did not reach the second provider broker")
	}

	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{
		Provider: secondProvider, State: "disconnected", Endpoint: secondBrokerPath,
	}); err != nil {
		t.Fatal(err)
	}
	d.credentialMu.Lock()
	disconnectedRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilitySSH)]
	d.credentialMu.Unlock()
	if disconnectedRoute != stableRoute {
		t.Fatal("transport disconnect replaced the stable SSH listener")
	}
	currentRouteInfo, err := os.Lstat(routePath)
	if err != nil {
		t.Fatalf("stable SSH route disappeared after transport disconnect: %v", err)
	}
	if !os.SameFile(stableRouteInfo, currentRouteInfo) {
		t.Fatal("transport disconnect replaced the stable SSH socket")
	}
	client, err := net.DialTimeout("unix", routePath, time.Second)
	if err != nil {
		t.Fatalf("dial stable SSH route after transport disconnect: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte{'c'}); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(client, reply[:]); err == nil {
		t.Fatalf("disconnected SSH route returned %q", reply[0])
	}
}

func TestStableOpenPGPRouteSwitchesBetweenRemoteProviders(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.OpenPGP.Enable = true
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	workspace := createTestWorkspace(t, d, 1)
	readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	routePath := filepath.Join(d.paths.RuntimeDir, "openpgp-route.sock")
	d.credentialMu.Lock()
	d.credentialRoutePaths[workspace.ID] = routePath
	d.credentialMu.Unlock()

	firstProvider := Host{ID: "first-openpgp-provider", Name: "first"}
	firstBrokerPath := filepath.Join(d.paths.RuntimeDir, "first-openpgp-broker.sock")
	firstBroker, err := listenUnix(firstBrokerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstBroker.Close() })
	firstHellos := make(chan credentialStreamHello, 1)
	go serveCredentialTestBroker(firstBroker, 'A', firstHellos)
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: firstProvider, State: "ready", Endpoint: firstBrokerPath}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].CredentialClaim = &CredentialClaim{
		ProviderSource: "remote", Bundle: "work", OwnerNodeID: firstProvider.ID, Generation: 1, State: "ready",
		Capabilities: map[string]CredentialCapabilityStatus{credentialCapabilityOpenPGP: {State: "ready", Available: true}},
	}
	d.mu.Unlock()
	d.reconcileCredentialRoutes(context.Background())
	d.credentialMu.Lock()
	stableRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilityOpenPGP)]
	d.credentialMu.Unlock()
	if stableRoute == nil {
		t.Fatal("first OpenPGP provider did not publish a stable route")
	}
	if got := credentialTestRoundTrip(t, routePath, 'a'); got != 'A' {
		t.Fatalf("first OpenPGP provider reply = %q", got)
	}
	if hello := <-firstHellos; hello.Capability != credentialCapabilityOpenPGP || hello.Generation != 1 {
		t.Fatalf("first OpenPGP hello = %#v", hello)
	}

	secondProvider := Host{ID: "second-openpgp-provider", Name: "second"}
	secondBrokerPath := filepath.Join(d.paths.RuntimeDir, "second-openpgp-broker.sock")
	secondBroker, err := listenUnix(secondBrokerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondBroker.Close() })
	secondHellos := make(chan credentialStreamHello, 1)
	go serveCredentialTestBroker(secondBroker, 'B', secondHellos)
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: secondProvider, State: "ready", Endpoint: secondBrokerPath}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].CredentialClaim = &CredentialClaim{
		ProviderSource: "remote", Bundle: "work", OwnerNodeID: secondProvider.ID, Generation: 2, State: "ready",
		Capabilities: map[string]CredentialCapabilityStatus{credentialCapabilityOpenPGP: {State: "ready", Available: true}},
	}
	d.mu.Unlock()
	d.revokeCredentialRoutes(workspace.ID, 1)
	d.reconcileCredentialRoutes(context.Background())
	d.credentialMu.Lock()
	transferredRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilityOpenPGP)]
	d.credentialMu.Unlock()
	if transferredRoute != stableRoute {
		t.Fatal("OpenPGP remote-to-remote transfer replaced the stable listener")
	}
	if got := credentialTestRoundTrip(t, routePath, 'b'); got != 'B' {
		t.Fatalf("second OpenPGP provider reply = %q", got)
	}
	if hello := <-secondHellos; hello.Capability != credentialCapabilityOpenPGP || hello.Generation != 2 {
		t.Fatalf("second OpenPGP hello = %#v", hello)
	}
}

func TestCredentialTransferClosesActiveOldGenerationStream(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-transfer-owner", d.state.Node.ID)
	localAgentPath := filepath.Join(d.paths.RuntimeDir, "long-lived-local-agent.sock")
	localAgent, err := listenUnix(localAgentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localAgent.Close() })
	go func() {
		for {
			conn, acceptErr := localAgent.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, localAgentPath, localOwner.ID, 0); err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTimeout("unix", agentRelaySocketPath(d.paths.AgentDir, workspace.ID), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	d.credentialMu.Lock()
	route := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilitySSH)]
	d.credentialMu.Unlock()
	activeBeforeTransfer := 0
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		route.mu.Lock()
		activeBeforeTransfer = len(route.active)
		route.mu.Unlock()
		if activeBeforeTransfer == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if activeBeforeTransfer != 1 {
		t.Fatalf("active streams before transfer = %d, want 1", activeBeforeTransfer)
	}

	provider := Host{ID: "replacement-provider", Name: "replacement"}
	providerAttachment := readyCredentialAttachment(t, d, workspace, "replacement-owner", provider.ID)
	brokerPath := filepath.Join(d.paths.RuntimeDir, "replacement-provider.sock")
	broker, err := listenUnix(brokerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{Provider: provider, State: "ready", Endpoint: brokerPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: "work", Provider: provider, ProviderSource: "remote",
		OwnerAttachmentID: providerAttachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", SSH: true},
	}); err != nil {
		t.Fatal(err)
	}
	route.mu.Lock()
	activeAfterTransfer := len(route.active)
	route.mu.Unlock()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var reply [1]byte
	if _, err := client.Read(reply[:]); err == nil {
		t.Fatal("old-generation stream remained open after provider transfer")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("old-generation stream timed out instead of being revoked; active after transfer=%d", activeAfterTransfer)
	}
}

func serveCredentialTestBroker(listener net.Listener, reply byte, hellos chan<- credentialStreamHello) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			hello, helloErr := readCredentialStreamHello(conn)
			if helloErr != nil {
				return
			}
			hellos <- hello
			var request [1]byte
			if _, readErr := io.ReadFull(conn, request[:]); readErr == nil {
				_, _ = conn.Write([]byte{reply})
			}
		}()
	}
}

func serveCredentialTestByte(listener net.Listener, reply byte) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var request [1]byte
			if _, readErr := io.ReadFull(conn, request[:]); readErr == nil {
				_, _ = conn.Write([]byte{reply})
			}
		}()
	}
}

func credentialTestRoundTrip(t *testing.T, path string, request byte) byte {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{request}); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	return reply[0]
}
