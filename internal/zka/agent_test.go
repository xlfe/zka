package zka

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

func TestPaneHostPropagatesExitAndReportsError(t *testing.T) {
	root := t.TempDir()
	d, err := newTestDaemon(t, root, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	workspace := createTestWorkspace(t, d, 1)
	pane := firstPane(workspace)
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].Panes[pane.ID].CWD = root
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	t.Setenv("ZKA_TEST_HELPER", "17")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	code, err := runPaneHost(
		[]string{"--workspace", workspace.ID, "--pane", pane.ID, "--", exe, "-test.run=TestAgentHelperProcess"},
		d.paths, os.Stdin, io.Discard, io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != 17 {
		t.Fatalf("exit code = %d", code)
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	reported := got.Panes[pane.ID]
	if reported.State != StateError || reported.Process.ExitCode == nil || *reported.Process.ExitCode != 17 || !reported.BackendCreated {
		t.Fatalf("reported = %#v", reported)
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
