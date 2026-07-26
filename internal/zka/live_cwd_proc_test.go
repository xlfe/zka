package zka

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fake process tree proves the policy; this proves the /proc reader that
// feeds it. It needs no shell: the test binary re-executes itself as a
// blocking child. It also exercises the multi-thread children union, because a
// Go test binary forks from an arbitrary thread.
func TestProcFSReadsLiveCWDOfAChildProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux-only; live cwd resolution degrades to the stored directory elsewhere")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(executable, "-test.run=TestLiveCWDHelperProcess")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ZKA_LIVE_CWD_HELPER=block")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	proc := procFS{}
	children, err := proc.Children(os.Getpid())
	if err != nil {
		t.Fatalf("read children of the test process: %v", err)
	}
	found := false
	for _, child := range children {
		if child == cmd.Process.Pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("child %d not among %v", cmd.Process.Pid, children)
	}

	cwd, err := proc.CWD(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("read child cwd: %v", err)
	}
	// readlink returns the fully resolved path, and /tmp is frequently a
	// symlink, so the expectation has to be resolved too.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cwd != want {
		t.Fatalf("child cwd = %q, want %q", cwd, want)
	}
}

func TestProcFSCmdlineSplitsOnNUL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux-only")
	}
	argv, err := procFS{}.Cmdline(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 || !strings.Contains(argv[0], ".test") {
		t.Fatalf("cmdline = %#v, want the test binary first", argv)
	}
	for _, arg := range argv {
		if arg == "" {
			t.Fatalf("cmdline retained an empty trailing field: %#v", argv)
		}
	}
}

// TestLiveCWDHelperProcess is not a test. It blocks so the parent can inspect
// it in /proc, and exits when its stdin closes.
func TestLiveCWDHelperProcess(t *testing.T) {
	if os.Getenv("ZKA_LIVE_CWD_HELPER") != "block" {
		t.Skip("helper process")
	}
	buffer := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buffer); err != nil {
			return
		}
	}
}
