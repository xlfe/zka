package zka

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pure-Go model in kittymodel_test.go is only trustworthy while it agrees
// with Kitty. This differential test drives Kitty's real session parser --
// headlessly, no display required -- and fails if the two disagree. It skips
// cleanly when no Kitty install is available, so it costs nothing in
// environments that lack one.
//
// Point it at a specific install with ZKA_KITTY_LIB=/path/to/lib/kitty.
func kittyLibDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("ZKA_KITTY_LIB"); dir != "" {
		return dir
	}
	binary, err := exec.LookPath("kitty")
	if err != nil {
		t.Skip("kitty is not installed; skipping the differential oracle")
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Skipf("resolve kitty binary: %v", err)
	}
	candidate := filepath.Join(filepath.Dir(filepath.Dir(resolved)), "lib", "kitty")
	if _, err := os.Stat(filepath.Join(candidate, "kitty", "session.py")); err != nil {
		t.Skipf("kitty session.py not found under %s", candidate)
	}
	return candidate
}

type oracleTab struct {
	Name           string   `json:"name"`
	Layout         string   `json:"layout"`
	EnabledLayouts []string `json:"enabled_layouts"`
	Windows        []struct {
		Title string   `json:"title"`
		CWD   string   `json:"cwd"`
		Args  []string `json:"args"`
	} `json:"windows"`
}

type oracleOSWindow struct {
	Class         string      `json:"class"`
	Name          string      `json:"name"`
	FocusOSWindow bool        `json:"focus_os_window"`
	FocusTab      *string     `json:"focus_tab"`
	Tabs          []oracleTab `json:"tabs"`
}

func runKittyOracle(t *testing.T, session string) []oracleOSWindow {
	t.Helper()
	libDir := kittyLibDir(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available; skipping the differential oracle")
	}
	path := filepath.Join(t.TempDir(), "oracle.kitty-session")
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "testdata/kitty_oracle.py", path)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+libDir)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("kitty rejected the rendered session: %v\n%s\nsession:\n%s", err, stderr, session)
	}
	var parsed []oracleOSWindow
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("decode oracle output: %v\n%s", err, out)
	}
	return parsed
}

// Real Kitty must reproduce the desired topology, and the pure-Go model must
// agree with real Kitty about what it produced.
func TestRealKittyReproducesDesiredTopology(t *testing.T) {
	cases := []struct {
		name string
		tabs []Node
	}{
		{"plain", []Node{{Kind: "tab", Layout: "splits", Children: paneNodes("a")}}},
		{"title with spaces", []Node{{Kind: "tab", Title: "Recovered 98b08d66", Layout: "splits", Children: paneNodes("a")}}},
		{"title with quotes", []Node{{Kind: "tab", Title: `has "quotes"`, Layout: "splits", Children: paneNodes("a")}}},
		{"title with dollar", []Node{{Kind: "tab", Title: "cost $HOME", Layout: "splits", Children: paneNodes("a")}}},
		{"title with apostrophe", []Node{{Kind: "tab", Title: "it's mine", Layout: "splits", Children: paneNodes("a")}}},
		{"title with hash", []Node{{Kind: "tab", Title: "#not a comment", Layout: "splits", Children: paneNodes("a")}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := roundTripWorkspace(t, testCase.tabs, []string{"a"})
			session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
			if err != nil {
				t.Fatal(err)
			}
			oracle := runKittyOracle(t, session)
			if len(oracle) != 1 || len(oracle[0].Tabs) != 1 {
				t.Fatalf("kitty produced %d os-windows: %#v", len(oracle), oracle)
			}
			want := workspace.Topology.Roots[0].Children[0]
			got := oracle[0].Tabs[0]
			// The exact bug: a quoted title came back with literal quotes.
			if got.Name != want.Title {
				t.Fatalf("real Kitty tab name = %q, desired %q\nsession:\n%s", got.Name, want.Title, session)
			}
			if got.Layout != want.Layout {
				t.Fatalf("real Kitty layout = %q, desired %q", got.Layout, want.Layout)
			}
			if len(got.Windows) != 1 {
				t.Fatalf("real Kitty produced %d windows, want 1 (an extra one is an unmanaged shell)", len(got.Windows))
			}

			// Now hold the model to the same answer.
			model := &fakeKitty{}
			if err := model.LoadSession(session); err != nil {
				t.Fatalf("model rejected a session real Kitty accepted: %v", err)
			}
			tree := model.LS()
			if modelName := tree[0].Tabs[0].namedTitle(); modelName != got.Name {
				t.Fatalf("model and real Kitty disagree on the tab name: model %q, kitty %q", modelName, got.Name)
			}
			if len(tree[0].Tabs[0].Windows) != len(got.Windows) {
				t.Fatalf("model and real Kitty disagree on window count: model %d, kitty %d",
					len(tree[0].Tabs[0].Windows), len(got.Windows))
			}
		})
	}
}

// focus_tab is per-OS-window in Kitty, because it replaces its session object at
// every new_os_window. Emitting it once at the end applied the index computed
// for one window to a different one.
func TestRealKittyReceivesPerOSWindowFocus(t *testing.T) {
	// Two OS windows, one pane each.
	workspace := roundTripWorkspace(t, []Node{
		{Kind: "tab", Layout: "splits", Children: paneNodes("a")},
	}, []string{"a"})
	workspace.Panes["b"] = &Pane{
		ID: "b", Position: 1, Title: "shell", CWD: "/work", State: StateUnknown,
		Phase: PaneAdmitted, Backend: BackendRef{Kind: "zmx", Ref: "b"},
	}
	roots := append(cloneNodes(workspace.Topology.Roots), Node{Kind: "os-window", Children: []Node{
		{Kind: "tab", Layout: "splits", Children: paneNodes("b")},
	}})
	if _, err := installDesiredTopology(workspace, roots, topologyInstallSystem); err != nil {
		t.Fatal(err)
	}
	session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(session, "focus_os_window") > 1 {
		t.Fatalf("focus_os_window emitted more than once:\n%s", session)
	}
	oracle := runKittyOracle(t, session)
	if len(oracle) != 2 {
		t.Fatalf("kitty produced %d os-windows, want 2:\n%s", len(oracle), session)
	}
	for index, osWindow := range oracle {
		if len(osWindow.Tabs) != 1 {
			t.Fatalf("os-window %d has %d tabs, want 1", index, len(osWindow.Tabs))
		}
		if len(osWindow.Tabs[0].Windows) != 1 {
			t.Fatalf("os-window %d tab has %d windows; an extra one is an unmanaged shell", index, len(osWindow.Tabs[0].Windows))
		}
	}
}

// Working-directory inheritance leans on exactly one undocumented Kitty
// behaviour: @active-kitty-window-id in a launch command resolves to the window
// that was active when the launch began. If a future Kitty drops it, the
// feature degrades silently to "no hint" -- correct, but invisible. This makes
// that regression loud instead.
func TestRealKittySubstitutesTheActiveWindowPlaceholder(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(kittyLibDir(t), "kitty", "launch.py"))
	if err != nil {
		t.Skipf("kitty launch.py is unreadable: %v", err)
	}
	if !strings.Contains(string(source), "'@active-kitty-window-id'") {
		t.Fatal("kitty no longer substitutes @active-kitty-window-id; new panes will stop inheriting a directory")
	}
}
