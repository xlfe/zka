package zka

import (
	"bytes"
	"context"
	"testing"
)

func TestDoctorOriginReportsDaemonAndCallerSSHAgents(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1234/agent-a.socket")
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.SSH.Command = "/definitely/missing/zka-ssh"
	serveTestDaemon(t, d)
	t.Setenv("SSH_AUTH_SOCK", "/run/user/1234/agent-b.socket")

	var stdout, stderr bytes.Buffer
	_, err = runDoctor([]string{"--origin", "devbox.example"}, d.paths, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"zkad-ssh-agent", "caller-ssh-agent", "ssh-agent-match", "/run/user/1234/agent-a.socket", "/run/user/1234/agent-b.socket"} {
		if !bytes.Contains(stdout.Bytes(), []byte(expected)) {
			t.Fatalf("doctor output missing %q: %s", expected, stdout.String())
		}
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
