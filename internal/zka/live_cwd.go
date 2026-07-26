package zka

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A pane's stored CWD is where it was created, and Kitty cannot tell us where
// its shell is now: Kitty's direct child is `zka pane`, whose cwd never moves,
// and the real shell lives two layers down inside zmx. Kitty's own
// last_reported cwd needs shell integration it never injects for a non-shell
// program, and it refuses to use it at all for ssh-backed panes.
//
// So zka resolves the live directory itself, from /proc on whichever daemon
// owns the workspace. That works for local and remote panes alike, for panes
// whose backend predates this code, and without depending on zmx forwarding
// escape sequences -- zmx embeds a VT parser and rewrites some of them.

// processTree is the slice of /proc this needs, behind an interface so the
// resolver is testable without real processes.
type processTree interface {
	Cmdline(pid int) ([]string, error)
	Children(pid int) ([]int, error)
	CWD(pid int) (string, error)
}

// procFS reads the real /proc. Root is overridable for tests.
type procFS struct{ Root string }

func (p procFS) root() string {
	if p.Root == "" {
		return "/proc"
	}
	return p.Root
}

func (p procFS) Cmdline(pid int) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	argv := strings.Split(string(raw), "\x00")
	for len(argv) != 0 && argv[len(argv)-1] == "" {
		argv = argv[:len(argv)-1]
	}
	return argv, nil
}

// Children unions <pid>/task/<tid>/children across threads: the entry belongs
// to the thread that forked, and Go programs fork from arbitrary threads. On
// kernels built without CONFIG_PROC_CHILDREN the file is absent, so fall back
// to scanning for processes whose PPid is pid -- PPid reports the parent's
// tgid, so it is correct whichever thread forked.
func (p procFS) Children(pid int) ([]int, error) {
	threads, err := os.ReadDir(filepath.Join(p.root(), strconv.Itoa(pid), "task"))
	if err != nil {
		return p.childrenByParentScan(pid)
	}
	seen := map[int]bool{}
	var children []int
	found := false
	for _, thread := range threads {
		raw, err := os.ReadFile(filepath.Join(p.root(), strconv.Itoa(pid), "task", thread.Name(), "children"))
		if err != nil {
			continue
		}
		found = true
		for _, field := range strings.Fields(string(raw)) {
			child, err := strconv.Atoi(field)
			if err != nil || seen[child] {
				continue
			}
			seen[child] = true
			children = append(children, child)
		}
	}
	if !found {
		return p.childrenByParentScan(pid)
	}
	return children, nil
}

func (p procFS) childrenByParentScan(pid int) ([]int, error) {
	entries, err := os.ReadDir(p.root())
	if err != nil {
		return nil, err
	}
	want := "PPid:\t" + strconv.Itoa(pid)
	var children []int
	for _, entry := range entries {
		candidate, err := strconv.Atoi(entry.Name())
		if err != nil || candidate == pid {
			continue
		}
		status, err := os.ReadFile(filepath.Join(p.root(), entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "PPid:") {
				continue
			}
			if strings.TrimRight(line, " \t\r") == want {
				children = append(children, candidate)
			}
			break
		}
	}
	return children, nil
}

func (p procFS) CWD(pid int) (string, error) {
	return os.Readlink(filepath.Join(p.root(), strconv.Itoa(pid), "cwd"))
}

// usableDirectory reports whether a recorded directory can still be entered.
// exec fails the whole launch when Dir does not exist, so an unusable value
// has to become "no directory" rather than a dead pane. A readlink of a
// removed directory comes back with a " (deleted)" suffix.
func usableDirectory(path string) bool {
	if path == "" || !filepath.IsAbs(path) || strings.HasSuffix(path, " (deleted)") {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// isPaneHostCmdline verifies argv is this pane's `zka pane-host`. Pane ids are
// 32 random hex characters, so checking the pair makes a recycled pid
// impossible to mistake for the real host. Only arguments before the "--"
// terminator are scanned, so an argument of the user's shell cannot spoof it.
func isPaneHostCmdline(argv []string, workspaceID, paneID string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "zka" || argv[1] != "pane-host" {
		return false
	}
	workspaceSeen, paneSeen := false, false
	for index := 2; index < len(argv); index++ {
		if argv[index] == "--" {
			break
		}
		if index+1 >= len(argv) {
			break
		}
		switch argv[index] {
		case "--workspace":
			workspaceSeen = workspaceSeen || argv[index+1] == workspaceID
		case "--pane":
			paneSeen = paneSeen || argv[index+1] == paneID
		}
	}
	return workspaceSeen && paneSeen
}

// liveShellCWD returns the directory the pane's shell is sitting in right now,
// or "" when it cannot be established.
//
// The shell pid is derived rather than reported. zmx sessions outlive zka
// upgrades, so a pid pushed at startup would only ever exist for panes created
// after the upgrade, leaving every already-running pane permanently
// un-inheritable. Deriving works for all of them, and adds no event, no stored
// field, and no schema change. It relies on pane-host having exactly one child
// -- the shell -- for its whole life; if that ever stops being true, an
// ambiguous child count degrades to the next tier rather than guessing.
func liveShellCWD(proc processTree, workspaceID string, pane *Pane) string {
	if proc == nil || pane == nil || pane.Process.PID <= 0 || !pane.Process.Running {
		return ""
	}
	argv, err := proc.Cmdline(pane.Process.PID)
	if err != nil || !isPaneHostCmdline(argv, workspaceID, pane.ID) {
		return ""
	}
	children, err := proc.Children(pane.Process.PID)
	if err != nil || len(children) != 1 {
		return ""
	}
	cwd, err := proc.CWD(children[0])
	if err != nil {
		return ""
	}
	return cwd
}

// inheritedPaneCWD is the whole policy, in one ordered ladder. It returns a
// string and never an error: every tier that fails falls through to the next,
// and the last tier is exactly the behaviour zka had before inheritance
// existed, so nothing here can fail pane creation.
func inheritedPaneCWD(proc processTree, workspaceID string, source *Pane, requested string) string {
	if live := liveShellCWD(proc, workspaceID, source); usableDirectory(live) {
		return live
	}
	if source != nil && usableDirectory(source.CWD) {
		return source.CWD
	}
	// Deliberately unvalidated: this is the caller's own cwd, and validating it
	// here would change existing behaviour for panes that never opted in.
	return requested
}
