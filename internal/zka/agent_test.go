package zka

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

func TestAgentRunPropagatesExitAndReportsError(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx) }()
	waitFor(t, func() bool {
		_, err := os.Stat(d.paths.Socket)
		return err == nil
	})
	session, err := d.createSession(createSessionRequest{Name: "runner", Command: []string{"codex"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZKA_TEST_HELPER", "17")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	code, err := runAgent(
		[]string{"--session", session.ID, "--", exe, "-test.run=TestAgentHelperProcess"},
		d.paths,
		os.Stdin,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != 17 {
		t.Fatalf("exit code = %d", code)
	}
	got, err := d.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateError || got.Process.ExitCode == nil || *got.Process.ExitCode != 17 || !got.BackendCreated {
		t.Fatalf("reported session = %#v", got)
	}
}

func TestAgentHelperProcess(t *testing.T) {
	value := os.Getenv("ZKA_TEST_HELPER")
	if value == "" {
		return
	}
	if value == "term" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {}
	}
	code, err := strconv.Atoi(value)
	if err != nil {
		os.Exit(125)
	}
	os.Exit(code)
}

func TestProcessExitCodeForSignal(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestAgentHelperProcess")
	cmd.Env = append(os.Environ(), "ZKA_TEST_HELPER=term")
	err = cmd.Run()
	if got := processExitCode(err); got != 128+int(syscall.SIGTERM) {
		t.Fatalf("signal exit code = %d", got)
	}
}
