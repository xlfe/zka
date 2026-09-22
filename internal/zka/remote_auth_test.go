package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestRemoteAuthenticationRequiresExplicitRetry(t *testing.T) {
	for _, mode := range []string{"refused", "timeout", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			d, modePath, attemptsPath := authenticationTestDaemon(t, mode)
			api := NewAPI(d.paths)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if mode == "cancelled" {
				go func() {
					// Cancel only after the helper has begun the signing attempt.
					for ctx.Err() == nil {
						if data, _ := os.ReadFile(attemptsPath); len(data) != 0 {
							cancel()
							return
						}
						time.Sleep(time.Millisecond)
					}
				}()
			}
			if err := api.RemoteCall(ctx, "origin.test", "list", nil, nil); err == nil {
				t.Fatal("unanswered authentication succeeded")
			}
			// Local API cancellation may return before the daemon releases its call.
			deadline := time.Now().Add(time.Second)
			for d.remotes.credentialTransportStatusForHost("origin.test").State != "authentication_required" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			assertAuthenticationRequired(t, d)
			assertSSHAttempts(t, attemptsPath, "authenticate\n")

			// Keep a durable provider claim active and exercise both recovery and
			// direct background callers, which used to recreate discarded clients.
			d.mu.Lock()
			d.state.Workspaces[remoteWorkspaceIDForTest] = &Workspace{
				ID: remoteWorkspaceIDForTest, RemoteHost: "origin.test",
				CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: d.state.Node.ID},
			}
			d.mu.Unlock()
			background, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			for range 5 {
				d.reconnectCredentialProviders(background)
				if _, err := d.remotes.Call(background, "origin.test", "list", nil); err == nil {
					t.Fatal("background call cleared authentication failure")
				}
				if err := api.RemoteCallBackground(background, "origin.test", "list", nil, nil); err == nil {
					t.Fatal("background API call cleared authentication failure")
				}
			}
			assertSSHAttempts(t, attemptsPath, "authenticate\n")

			if err := os.WriteFile(modePath, []byte("success"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := api.RemoteCall(background, "origin.test", "list", nil, nil); err != nil {
				t.Fatalf("explicit reconnect: %v", err)
			}
			if err := api.RemoteCallBackground(background, "origin.test", "list", nil, nil); err != nil {
				t.Fatalf("reuse established control session: %v", err)
			}
			assertSSHAttempts(t, attemptsPath, "authenticate\nauthenticate\n")
		})
	}
}

func TestRemoteBackgroundStartupOnlyReusesSSH(t *testing.T) {
	for _, master := range []bool{false, true} {
		name := "missing_master"
		if master {
			name = "existing_master"
		}
		t.Run(name, func(t *testing.T) {
			d, modePath, attemptsPath := authenticationTestDaemon(t, "success")
			if master {
				if err := os.WriteFile(modePath+".master", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := d.remotes.Call(ctx, "origin.test", "list", nil)
			if master && err != nil {
				t.Fatalf("authenticated master was not reused: %v", err)
			}
			if !master {
				if err == nil {
					t.Fatal("missing master unexpectedly succeeded")
				}
				assertAuthenticationRequired(t, d)
				_, _ = d.remotes.Call(ctx, "origin.test", "list", nil)
			}
			assertSSHAttempts(t, attemptsPath, "reuse\n")
		})
	}
}

func TestRemoteDisconnectDoesNotAuthenticateAgain(t *testing.T) {
	for _, mode := range []string{"drop", "drop-reuse"} {
		t.Run(mode, func(t *testing.T) {
			d, _, attemptsPath := authenticationTestDaemon(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := NewAPI(d.paths).RemoteCall(ctx, "origin.test", "list", nil, nil)
			if mode == "drop-reuse" {
				if err != nil {
					t.Fatalf("recovery via authenticated master: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("reconnected without authenticated master")
				}
				assertAuthenticationRequired(t, d)
				_, _ = d.remotes.Call(ctx, "origin.test", "list", nil)
			}
			assertSSHAttempts(t, attemptsPath, "authenticate\nreuse\n")
		})
	}
}

func authenticationTestDaemon(t *testing.T, mode string) (*Daemon, string, string) {
	t.Helper()
	root := testRoot(t)
	modePath, attemptsPath := filepath.Join(root, "mode"), filepath.Join(root, "attempts")
	if err := os.WriteFile(modePath, []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_ZKA_SSH_HELPER", "authentication")
	t.Setenv("ZKA_TEST_SSH_MODE", modePath)
	t.Setenv("ZKA_TEST_SSH_ATTEMPTS", attemptsPath)
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = os.Args[0]
	d.config.SSH.Options = []string{"-test.run=TestZKASSHHelperProcess", "--", "-o", "ControlMaster=auto", "-o", "ProxyCommand=custom-proxy"}
	serveTestDaemon(t, d)
	return d, modePath, attemptsPath
}

func assertSSHAttempts(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("SSH attempts = %q, %v; want %q", got, err, want)
	}
}

func assertAuthenticationRequired(t *testing.T, d *Daemon) {
	t.Helper()
	status := d.remotes.credentialTransportStatusForHost("origin.test")
	if status.State != "authentication_required" || !status.NextRetryAt.IsZero() || !strings.Contains(status.LastError, "zka workspace list --origin origin.test") {
		t.Fatalf("transport state = %#v", status)
	}
}

// Simulate an SSH client and count the attempts that would ask an agent to
// sign. A reuse-only invocation cannot authenticate when the master is gone.
func runAuthenticationSSHHelper() {
	modePath := os.Getenv("ZKA_TEST_SSH_MODE")
	data, _ := os.ReadFile(modePath)
	mode := string(data)
	reuse := slices.Contains(os.Args, "ProxyCommand=false")
	attempt := "authenticate\n"
	if reuse {
		if !slices.Equal(os.Args[1:5], []string{"-o", "ControlMaster=no", "-o", "ProxyCommand=false"}) {
			os.Exit(3)
		}
		attempt = "reuse\n"
	}
	f, err := os.OpenFile(os.Getenv("ZKA_TEST_SSH_ATTEMPTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	_, _ = io.WriteString(f, attempt)
	_ = f.Close()
	if reuse {
		if _, err := os.Stat(modePath + ".master"); err != nil {
			_, _ = io.WriteString(os.Stderr, "kex_exchange_identification: Connection closed by remote host\n")
			os.Exit(255)
		}
	} else {
		switch mode {
		case "refused":
			_, _ = io.WriteString(os.Stderr, "sign_and_send_pubkey: signing failed: agent refused operation\n")
			os.Exit(255)
		case "timeout", "cancelled":
			for {
				time.Sleep(time.Hour)
			}
		}
	}
	session, err := yamux.Server(newStdioConn(os.Stdin, os.Stdout), remoteYamuxConfig())
	if err != nil {
		os.Exit(3)
	}
	control, err := session.AcceptStream()
	if err != nil {
		os.Exit(3)
	}
	encoder, reader := json.NewEncoder(control), bufio.NewReader(control)
	payload, _ := json.Marshal(remoteServerHello{Node: Host{ID: remoteWorkspaceIDForTest}})
	_ = encoder.Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: remoteProtocolVersion, Type: "hello", Payload: payload})
	_, _ = readRemoteEnvelope(reader)
	if !reuse && strings.HasPrefix(mode, "drop") {
		if mode == "drop-reuse" {
			_ = os.WriteFile(modePath+".master", nil, 0600)
		}
		_, _ = io.WriteString(os.Stderr, "client_loop: send disconnect: connection reset\n")
		os.Exit(255)
	}
	_ = encoder.Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: remoteProtocolVersion, Type: "client_hello_ack"})
	for {
		request, err := readRemoteEnvelope(reader)
		if err != nil {
			os.Exit(0)
		}
		_ = encoder.Encode(remoteEnvelope{Protocol: remoteProtocolName, Version: remoteProtocolVersion, Type: "response", ID: request.ID, Payload: json.RawMessage(`[]`)})
	}
}
