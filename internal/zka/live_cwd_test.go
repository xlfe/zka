package zka

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeProcessTree is a fixed process table. Any pid absent from a map errors,
// which is how "/proc is unavailable" is expressed.
type fakeProcessTree struct {
	cmdline  map[int][]string
	children map[int][]int
	cwd      map[int]string
	broken   bool
	cwdCalls int
	onCWD    func()
}

func (f *fakeProcessTree) Cmdline(pid int) ([]string, error) {
	if f.broken {
		return nil, errors.New("no /proc")
	}
	if argv, ok := f.cmdline[pid]; ok {
		return argv, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeProcessTree) Children(pid int) ([]int, error) {
	if f.broken {
		return nil, errors.New("no /proc")
	}
	if children, ok := f.children[pid]; ok {
		return children, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeProcessTree) CWD(pid int) (string, error) {
	f.cwdCalls++
	if f.onCWD != nil {
		f.onCWD()
	}
	if f.broken {
		return "", errors.New("no /proc")
	}
	if dir, ok := f.cwd[pid]; ok {
		return dir, nil
	}
	return "", os.ErrNotExist
}

func paneHostArgv(workspaceID, paneID string) []string {
	return []string{"/nix/store/abc/bin/zka", "pane-host", "--workspace", workspaceID, "--pane", paneID, "--", "fish"}
}

func livePane(paneID string, pid int) *Pane {
	return &Pane{ID: paneID, Phase: PaneAdmitted, Process: ProcessStatus{Running: true, PID: pid}}
}

// The whole point: a new pane starts where the source shell is now, not where
// the source pane was created.
func TestInheritedCWDPrefersLiveShellDirectory(t *testing.T) {
	live, stored := t.TempDir(), t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	proc := &fakeProcessTree{
		cmdline:  map[int][]string{100: paneHostArgv("ws", "pane1")},
		children: map[int][]int{100: {200}},
		cwd:      map[int]string{200: live},
	}
	if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != live {
		t.Fatalf("inherited cwd = %q, want the live shell directory %q", got, live)
	}
}

// A pid can be reused after the pane-host exits. The cmdline check is what
// stops an unrelated process being mistaken for this pane's shell.
func TestInheritedCWDRejectsRecycledPaneHostPID(t *testing.T) {
	stored := t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	proc := &fakeProcessTree{
		// Same pid, but it now hosts a different pane.
		cmdline:  map[int][]string{100: paneHostArgv("ws", "someone-else")},
		children: map[int][]int{100: {200}},
		cwd:      map[int]string{200: t.TempDir()},
	}
	if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != stored {
		t.Fatalf("inherited cwd = %q, want the stored directory %q", got, stored)
	}
	if proc.cwdCalls != 0 {
		t.Fatal("a recycled pid should be rejected before its cwd is read")
	}
}

// The user's shell gets arbitrary arguments. They must not be able to look
// like the pane identity.
func TestInheritedCWDIgnoresArgumentsAfterTheTerminator(t *testing.T) {
	stored := t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	spoofed := []string{
		"/bin/zka", "pane-host", "--workspace", "ws", "--pane", "other",
		"--", "fish", "-c", "--pane", "pane1",
	}
	proc := &fakeProcessTree{
		cmdline:  map[int][]string{100: spoofed},
		children: map[int][]int{100: {200}},
		cwd:      map[int]string{200: t.TempDir()},
	}
	if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != stored {
		t.Fatalf("a shell argument spoofed the pane identity: %q", got)
	}
}

func TestInheritedCWDSkipsUnusableDirectories(t *testing.T) {
	removed := filepath.Join(t.TempDir(), "gone")
	source := livePane("pane1", 100)
	source.CWD = removed
	proc := &fakeProcessTree{
		cmdline:  map[int][]string{100: paneHostArgv("ws", "pane1")},
		children: map[int][]int{100: {200}},
		// Exactly what readlink reports for a deleted working directory.
		cwd: map[int]string{200: "/tmp/scratch (deleted)"},
	}
	if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != "/requested" {
		t.Fatalf("inherited cwd = %q, want the requested fallback", got)
	}
	if got := inheritedPaneCWD(proc, "ws", source, ""); got != "" {
		t.Fatalf("inherited cwd = %q, want empty when every tier is unusable", got)
	}
}

func TestInheritedCWDFallsBackWhenProcIsUnavailable(t *testing.T) {
	stored := t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	for name, proc := range map[string]processTree{
		"no /proc at all": &fakeProcessTree{broken: true},
		"unknown pid":     &fakeProcessTree{},
		"nil tree":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != stored {
				t.Fatalf("inherited cwd = %q, want the stored directory", got)
			}
		})
	}
}

func TestInheritedCWDRequiresARunningShell(t *testing.T) {
	stored := t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	source.Process.Running = false
	proc := &fakeProcessTree{
		cmdline:  map[int][]string{100: paneHostArgv("ws", "pane1")},
		children: map[int][]int{100: {200}},
		cwd:      map[int]string{200: t.TempDir()},
	}
	if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != stored {
		t.Fatalf("inherited cwd = %q, want the stored directory for a dead shell", got)
	}
}

// pane-host owning exactly one child is an invariant of how zka spawns the
// shell, not something the kernel enforces. If it is ever violated, guessing
// would be worse than falling through.
func TestInheritedCWDRejectsAmbiguousChildren(t *testing.T) {
	stored := t.TempDir()
	source := livePane("pane1", 100)
	source.CWD = stored
	for name, children := range map[string][]int{"none": {}, "two": {200, 201}} {
		t.Run(name, func(t *testing.T) {
			proc := &fakeProcessTree{
				cmdline:  map[int][]string{100: paneHostArgv("ws", "pane1")},
				children: map[int][]int{100: children},
				cwd:      map[int]string{200: t.TempDir(), 201: t.TempDir()},
			}
			if got := inheritedPaneCWD(proc, "ws", source, "/requested"); got != stored {
				t.Fatalf("inherited cwd = %q, want the stored directory", got)
			}
		})
	}
}

func TestInheritedCWDWithoutASourcePaneKeepsTheRequestedDirectory(t *testing.T) {
	if got := inheritedPaneCWD(&fakeProcessTree{}, "ws", nil, "/requested"); got != "/requested" {
		t.Fatalf("inherited cwd = %q, want the requested directory untouched", got)
	}
}

func TestUsableDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		dir:                           true,
		file:                          false,
		"":                            false,
		"relative/path":               false,
		filepath.Join(dir, "missing"): false,
		dir + " (deleted)":            false,
	}
	for path, want := range cases {
		if got := usableDirectory(path); got != want {
			t.Fatalf("usableDirectory(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsPaneHostCmdline(t *testing.T) {
	cases := map[string]struct {
		argv []string
		want bool
	}{
		"match":           {paneHostArgv("ws", "pane1"), true},
		"other pane":      {paneHostArgv("ws", "other"), false},
		"other workspace": {paneHostArgv("other", "pane1"), false},
		"not pane-host":   {[]string{"/bin/zka", "pane", "--workspace", "ws", "--pane", "pane1"}, false},
		"not zka":         {[]string{"/bin/fish", "pane-host", "--workspace", "ws", "--pane", "pane1"}, false},
		"empty":           {nil, false},
		"trailing flag":   {[]string{"/bin/zka", "pane-host", "--workspace"}, false},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isPaneHostCmdline(testCase.argv, "ws", "pane1"); got != testCase.want {
				t.Fatalf("isPaneHostCmdline = %v, want %v", got, testCase.want)
			}
		})
	}
}
