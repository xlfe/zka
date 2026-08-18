package zka

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// roundTripWorkspace builds a workspace whose desired topology has the given
// tab shape, then renders and replays it through the Kitty model.
func roundTripWorkspace(t *testing.T, tabs []Node, panes []string) *Workspace {
	t.Helper()
	workspace := &Workspace{
		ID: "workspace", Name: "work", Panes: map[string]*Pane{},
		Attachments: map[string]*Attachment{},
	}
	now := time.Now().UTC()
	for index, paneID := range panes {
		workspace.Panes[paneID] = &Pane{
			ID: paneID, Position: index, Title: "shell", CWD: "/work",
			State: StateUnknown, Phase: PaneAdmitted, PhaseAt: now,
			Backend: BackendRef{Kind: "zmx", Ref: paneID}, CreatedAt: now, UpdatedAt: now,
		}
	}
	if _, err := installDesiredTopology(workspace, []Node{{Kind: "os-window", Children: tabs}}, topologyInstallSystem); err != nil {
		t.Fatalf("install desired topology: %v", err)
	}
	return workspace
}

func paneNodes(paneIDs ...string) []Node {
	nodes := make([]Node, 0, len(paneIDs))
	for _, paneID := range paneIDs {
		nodes = append(nodes, Node{Kind: "pane", PaneID: paneID})
	}
	return nodes
}

// This is the test the outage slipped through. The desired topology must be a
// fixed point of Kitty's round trip: render it to a session file, let Kitty
// parse it, read the tree back, and the structure must be identical. A tab
// title containing a space used to come back wrapped in literal quote
// characters, so the reconciler rebuilt every window forever trying to fix a
// difference it had itself created.
func TestDesiredTopologyIsAFixedPointThroughKitty(t *testing.T) {
	layoutState := json.RawMessage(
		`{"all_windows":{"active_group_idx":0,"active_group_history":[],"window_groups":[{"id":1,"window_ids":[1]}]},"class":"Splits","opts":{}}`)
	cases := []struct {
		name  string
		tabs  []Node
		panes []string
	}{
		{"single pane", []Node{{Kind: "tab", Layout: "splits", Children: paneNodes("a")}}, []string{"a"}},
		{"two tabs", []Node{
			{Kind: "tab", Layout: "splits", Children: paneNodes("a")},
			{Kind: "tab", Layout: "tall", Children: paneNodes("b")},
		}, []string{"a", "b"}},
		{"split tab", []Node{
			{Kind: "tab", Layout: "splits", LayoutState: layoutState, Children: paneNodes("a", "b")},
		}, []string{"a", "b"}},
		{"title with spaces", []Node{
			{Kind: "tab", Title: "Recovered 98b08d66", Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"title with quotes", []Node{
			{Kind: "tab", Title: `has "quotes" inside`, Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"title with dollar", []Node{
			{Kind: "tab", Title: "cost $HOME and ${X}", Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"title with apostrophe", []Node{
			{Kind: "tab", Title: "it's mine", Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"title with backslash", []Node{
			{Kind: "tab", Title: `back\slash`, Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"title with hash", []Node{
			{Kind: "tab", Title: "#not a comment", Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
		{"unnamed tab", []Node{
			{Kind: "tab", Layout: "splits", Children: paneNodes("a")},
		}, []string{"a"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := roundTripWorkspace(t, testCase.tabs, testCase.panes)
			session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
			if err != nil {
				t.Fatalf("render session: %v", err)
			}
			kitty := &fakeKitty{}
			if err := kitty.LoadSession(session); err != nil {
				t.Fatalf("kitty rejected the rendered session: %v\n%s", err, session)
			}
			tree := kitty.LS()
			for _, osWindow := range tree {
				for _, tab := range osWindow.Tabs {
					for _, window := range tab.Windows {
						if window.UserVars["zka_pane"] == "" {
							t.Fatalf("session produced an untagged window, which breaks every later capture:\n%s", session)
						}
					}
				}
			}
			observed, err := topologyFromKitty(tree, workspace.ID)
			if err != nil {
				t.Fatalf("read topology back: %v\n%s", err, session)
			}
			observed, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, observed)
			if err != nil {
				t.Fatalf("stabilize: %v", err)
			}
			if !topologyMatchesDesired(workspace, observed) {
				t.Fatalf("desired topology is not reproducible by Kitty\nsession:\n%s\ndesired: %#v\nobserved: %#v",
					session, workspace.Topology.Roots, observed)
			}
			// Tab names must survive verbatim, which is the specific thing the
			// old quoting broke.
			for index, tabNode := range workspace.Topology.Roots[0].Children {
				got := tree[0].Tabs[index].namedTitle()
				if got != tabNode.Title {
					t.Fatalf("tab title round trip: sent %q, Kitty reported %q", tabNode.Title, got)
				}
			}
		})
	}
}

// The historical fabricated node, kept as a negative control: it documents why
// synthesizing topology is banned. A tab with no enabled_layouts and no
// layout_state is not something Kitty can ever report back.
func TestFabricatedRecoveryTabWasNeverReproducible(t *testing.T) {
	workspace := roundTripWorkspace(t, []Node{
		{Kind: "tab", Title: "Recovered abcd1234", Layout: "splits", Children: paneNodes("a")},
	}, []string{"a"})
	// Strip the fields a capture would have supplied, reproducing the shape
	// appendRecoveredPane used to install.
	fabricated := cloneNodes(workspace.Topology.Roots)
	fabricated[0].Children[0].EnabledLayouts = nil
	fabricated[0].Children[0].LayoutState = nil

	session, err := renderDesiredTopologySession(workspace, Transport{Kind: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	kitty := &fakeKitty{}
	if err := kitty.LoadSession(session); err != nil {
		t.Fatal(err)
	}
	observed, err := topologyFromKitty(kitty.LS(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed, err = stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, observed); err != nil {
		t.Fatal(err)
	}
	if len(observed[0].Children[0].EnabledLayouts) == 0 || len(observed[0].Children[0].LayoutState) == 0 {
		t.Fatal("model no longer reports enabled_layouts and layout_state; the negative control is meaningless")
	}
	// Under the structural digest these differences are correctly ignored, so
	// the node is reproducible again. Under the old full-metadata digest they
	// were identity-bearing, which is what made the tab unmatchable forever.
	if !topologyStructureEqual(fabricated, observed) {
		t.Fatal("structural identity should ignore presentation-only fields")
	}
	if nodesEqual(fabricated, observed) {
		t.Fatal("fixture no longer differs in presentation; test cannot detect a regression")
	}
}

// Every directive Kitty consumes verbatim must survive the round trip. Quoting
// any of them corrupts the value; `layout` and `enabled_layouts` additionally
// abort the whole session load.
func TestVerbatimDirectivesAreNotShellQuoted(t *testing.T) {
	var out sessionWriter
	out.NewTab("Recovered 98b08d66")
	out.EnabledLayouts([]string{"splits", "tall"})
	out.Layout("splits")
	out.OSWindowClass("my class")
	out.OSWindowName("my name")
	rendered := out.String()
	for _, forbidden := range []string{`new_tab "`, `layout "`, `enabled_layouts "`, `os_window_class "`, `os_window_name "`} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("directive was shell-quoted, which Kitty takes literally:\n%s", rendered)
		}
	}
	lines := parseSessionLines(rendered)
	if lines[0].Directive != "new_tab" || lines[0].verbatimValue() != "Recovered 98b08d66" {
		t.Fatalf("new_tab round trip = %#v", lines[0])
	}
}

// Launch options are expanded by Kitty and program arguments are not, so only
// the former may be "$"-escaped. Getting this wrong doubled every "$" on each
// capture and left the doubling in program arguments forever.
func TestLaunchLineEscapesOptionsButNotArguments(t *testing.T) {
	line := LaunchLine{
		Options: launchOptions{{Name: "--var", Value: "note=cost $5"}},
		Args:    []string{"zka", "remote-pane", "--origin", "ho$t"},
	}
	rendered := line.SessionLine()
	parsed, err := parseZkaLaunchLine(rendered)
	if err != nil {
		t.Fatalf("parse %q: %v", rendered, err)
	}
	if got := parsed.Options.VarValue("--var", "note"); got != "cost $5" {
		t.Fatalf("option value round trip = %q", got)
	}
	if parsed.Args[3] != "ho$t" {
		t.Fatalf("argument round trip = %q", parsed.Args[3])
	}
	// Repeated capture/render cycles must be a fixed point, not a ratchet.
	for i := 0; i < 3; i++ {
		rendered = parsed.SessionLine()
		parsed, err = parseZkaLaunchLine(rendered)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := parsed.Options.VarValue("--var", "note"); got != "cost $5" {
		t.Fatalf("option value drifted across cycles = %q", got)
	}
	if parsed.Args[3] != "ho$t" {
		t.Fatalf("argument drifted across cycles = %q", parsed.Args[3])
	}
}

// Kitty splits a session file on more than "\n", strips directive values twice,
// and expands variables. A value that does not survive that is unobservable and
// must never enter the desired state.
func TestCanonicalValuesAreFixedPointsOfKittysParsing(t *testing.T) {
	for _, raw := range []string{
		"  leading and trailing  ", "line break", "vertical\vtab",
		"form\ffeed", "sep\x1cchar", "nextline", "plain",
	} {
		canonical := canonicalStrippedValue(raw)
		if canonicalStrippedValue(canonical) != canonical {
			t.Fatalf("canonicalisation is not idempotent for %q", raw)
		}
		var out sessionWriter
		out.NewTab(canonical)
		lines := parseSessionLines(out.String())
		if len(lines) != 1 {
			t.Fatalf("%q produced %d session lines, so Kitty would see a different tab", raw, len(lines))
		}
		if got := lines[0].verbatimValue(); got != canonical {
			t.Fatalf("value %q came back as %q", canonical, got)
		}
	}
}
