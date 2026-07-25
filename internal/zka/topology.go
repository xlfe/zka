package zka

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const recoveredTabPrefix = "Recovered "

func activeTopologyPaneIDs(workspace *Workspace) map[string]bool {
	result := map[string]bool{}
	for id, pane := range workspace.Panes {
		if !pane.RemovalPending && !pane.TopologyPending {
			result[id] = true
		}
	}
	return result
}

func desiredPaneIDs(workspace *Workspace) map[string]bool {
	if len(workspace.Topology.Roots) != 0 {
		return topologyPaneIDs(workspace.Topology.Roots)
	}
	return manifestPaneIDsLegacy(workspace)
}

func manifestPaneIDsLegacy(workspace *Workspace) map[string]bool {
	result := topologyPaneIDs(workspace.Manifest.Topology)
	if len(result) == 0 {
		for id, pane := range workspace.Panes {
			if pane.Visible && !pane.RemovalPending {
				result[id] = true
			}
		}
	}
	return result
}

func canonicalTopology(nodes []Node) []Node {
	result := cloneNodes(nodes)
	var normalize func([]Node)
	normalize = func(children []Node) {
		for index := range children {
			node := &children[index]
			node.Active = false
			node.Focused = false
			if node.Kind == "os-window" {
				node.State = ""
			}
			node.LayoutState = stableLayoutState(node.LayoutState)
			normalize(node.Children)
		}
	}
	normalize(result)
	return result
}

// Captured Kitty layout state is rewritten to stable pane-derived IDs before
// reaching this function. Re-encoding here provides deterministic object-key
// ordering for digests and persisted desired topology.
func stableLayoutState(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return normalized
}

func topologyDigest(nodes []Node) string {
	digestNodes := topologyIdentity(nodes)
	encoded, _ := json.Marshal(digestNodes)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func topologyIdentity(nodes []Node) []Node {
	digestNodes := canonicalTopology(nodes)
	var normalizeLayout func([]Node)
	normalizeLayout = func(children []Node) {
		for index := range children {
			switch children[index].Kind {
			case "os-window":
				children[index].State = ""
			case "pane":
				// The running shell owns cwd independently on each replica.
				// Origin metadata is used for new panes and cold restoration.
				children[index].CWD = ""
			}
			children[index].LayoutState = stableLayoutState(children[index].LayoutState)
			normalizeLayout(children[index].Children)
		}
	}
	normalizeLayout(digestNodes)
	return digestNodes
}

func topologyMatchesDesired(workspace *Workspace, nodes []Node) bool {
	return topologyDigest(nodes) == workspace.Topology.Digest &&
		samePaneSet(topologyPaneIDs(nodes), desiredPaneIDs(workspace))
}

func deterministicTopologyID(workspaceID, kind, identity string) string {
	sum := sha256.Sum256([]byte(workspaceID + "\x00" + kind + "\x00" + identity))
	return hex.EncodeToString(sum[:12])
}

func paneSignature(node Node) string {
	ids := topologyPaneIDs([]Node{node})
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

// stabilizeTopologyIDs retains container IDs by descendant pane membership.
// A largest-overlap fallback preserves a tab ID when a pane is added or moved.
func stabilizeTopologyIDs(workspaceID string, previous, captured []Node) ([]Node, error) {
	available := map[string][]Node{}
	var collect func([]Node)
	collect = func(nodes []Node) {
		for _, node := range nodes {
			if node.Kind != "pane" && node.ID != "" {
				available[node.Kind] = append(available[node.Kind], node)
			}
			collect(node.Children)
		}
	}
	collect(previous)
	used := map[string]bool{}
	var assign func([]Node, string) ([]Node, error)
	assign = func(nodes []Node, path string) ([]Node, error) {
		result := cloneNodes(nodes)
		for index := range result {
			node := &result[index]
			switch node.Kind {
			case "pane":
				if node.PaneID == "" {
					return nil, fmt.Errorf("topology pane has no pane id")
				}
				node.ID = node.PaneID
			case "tab", "os-window":
				signature := paneSignature(*node)
				bestID, bestScore := "", -1
				if node.ID != "" && !used[node.ID] {
					bestID, bestScore = node.ID, 1<<21
				}
				for _, candidate := range available[node.Kind] {
					if used[candidate.ID] {
						continue
					}
					score := paneOverlap(*node, candidate)
					if paneSignature(candidate) == signature {
						score += 1 << 20
					}
					if node.ID != "" && candidate.ID == node.ID {
						score += 1 << 22
					}
					if score > bestScore || (score == bestScore && candidate.ID < bestID) {
						bestID, bestScore = candidate.ID, score
					}
				}
				if bestScore <= 0 {
					bestID = deterministicTopologyID(workspaceID, node.Kind, path+"/"+strconv.Itoa(index)+"/"+signature)
				}
				node.ID = bestID
				used[bestID] = true
			default:
				return nil, fmt.Errorf("unsupported topology node kind %q", node.Kind)
			}
			children, err := assign(node.Children, path+"/"+node.ID)
			if err != nil {
				return nil, err
			}
			node.Children = children
		}
		return result, nil
	}
	return assign(captured, "")
}

func paneOverlap(left, right Node) int {
	leftIDs := topologyPaneIDs([]Node{left})
	rightIDs := topologyPaneIDs([]Node{right})
	score := 0
	for id := range leftIDs {
		if rightIDs[id] {
			score++
		}
	}
	return score
}

type topologyIndex struct {
	nodes  map[string]Node
	parent map[string]string
	order  map[string][]string
}

func indexTopology(nodes []Node) (topologyIndex, error) {
	index := topologyIndex{
		nodes:  map[string]Node{},
		parent: map[string]string{},
		order:  map[string][]string{},
	}
	var visit func([]Node, string) error
	visit = func(children []Node, parent string) error {
		for _, node := range children {
			if node.ID == "" {
				return fmt.Errorf("cannot index topology node without an id")
			}
			if _, exists := index.nodes[node.ID]; exists {
				return fmt.Errorf("cannot index duplicate topology node %s", node.ID)
			}
			copy := node
			copy.Children = nil
			index.nodes[node.ID] = copy
			index.parent[node.ID] = parent
			index.order[parent] = append(index.order[parent], node.ID)
			if err := visit(node.Children, node.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return index, visit(nodes, "")
}

func topologyNodeMetadataEqual(left, right Node) bool {
	left.Children, right.Children = nil, nil
	left.Active, right.Active = false, false
	left.Focused, right.Focused = false, false
	if left.Kind == "pane" {
		left.CWD, right.CWD = "", ""
	}
	if left.Kind == "os-window" {
		left.State, right.State = "", ""
	}
	left.LayoutState = stableLayoutState(left.LayoutState)
	right.LayoutState = stableLayoutState(right.LayoutState)
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// mergeTopologyNodeMetadata performs a field-level three-way merge. Replacing
// the whole node would let a stale capture reset unrelated metadata changed by
// another attachment after the capture's baseline.
func mergeTopologyNodeMetadata(current, baseline, captured Node) Node {
	if captured.Title != baseline.Title {
		current.Title = captured.Title
	}
	if captured.Kind != "pane" && captured.CWD != baseline.CWD {
		current.CWD = captured.CWD
	}
	if captured.Kind != "os-window" && captured.State != baseline.State {
		current.State = captured.State
	}
	if captured.Class != baseline.Class {
		current.Class = captured.Class
	}
	if captured.Name != baseline.Name {
		current.Name = captured.Name
	}
	if captured.Layout != baseline.Layout {
		current.Layout = captured.Layout
	}
	if !slices.Equal(captured.EnabledLayouts, baseline.EnabledLayouts) {
		current.EnabledLayouts = append([]string(nil), captured.EnabledLayouts...)
	}
	if string(stableLayoutState(captured.LayoutState)) != string(stableLayoutState(baseline.LayoutState)) {
		current.LayoutState = append(json.RawMessage(nil), captured.LayoutState...)
	}
	return current
}

// rebaseCapturedTopology applies only changes observable between an
// attachment's last verified tree and its new capture. Nodes concurrently
// added to the canonical tree are retained. Nodes concurrently closed at the
// origin are never resurrected.
func rebaseCapturedTopology(workspace *Workspace, baseline, captured []Node) ([]Node, error) {
	currentIndex, err := indexTopology(workspace.Topology.Roots)
	if err != nil {
		return nil, err
	}
	baselineIndex, err := indexTopology(baseline)
	if err != nil {
		return nil, err
	}
	capturedIndex, err := indexTopology(captured)
	if err != nil {
		return nil, err
	}

	for id, capturedNode := range capturedIndex.nodes {
		baselineNode, existedAtBaseline := baselineIndex.nodes[id]
		currentNode, existsNow := currentIndex.nodes[id]
		if !existsNow && existedAtBaseline {
			// A concurrent close wins over stale edits.
			continue
		}
		if !existsNow {
			currentNode = capturedNode
			currentIndex.nodes[id] = currentNode
		}
		if !existedAtBaseline || !topologyNodeMetadataEqual(baselineNode, capturedNode) {
			if existedAtBaseline {
				currentNode = mergeTopologyNodeMetadata(currentNode, baselineNode, capturedNode)
			} else {
				currentNode = capturedNode
			}
			currentNode.Children = nil
			currentIndex.nodes[id] = currentNode
		}
		if !existedAtBaseline || baselineIndex.parent[id] != capturedIndex.parent[id] {
			targetParent := capturedIndex.parent[id]
			if targetParent == "" {
				currentIndex.parent[id] = ""
			} else if _, targetExists := currentIndex.nodes[targetParent]; targetExists {
				currentIndex.parent[id] = targetParent
			}
		}
	}
	for id := range capturedIndex.nodes {
		_, existedAtBaseline := baselineIndex.nodes[id]
		if _, existsNow := currentIndex.nodes[id]; !existsNow {
			continue
		}
		if existedAtBaseline && baselineIndex.parent[id] == capturedIndex.parent[id] {
			continue
		}
		targetParent := capturedIndex.parent[id]
		if targetParent == "" {
			currentIndex.parent[id] = ""
		} else if _, targetExists := currentIndex.nodes[targetParent]; targetExists {
			currentIndex.parent[id] = targetParent
		}
	}

	for parent, capturedOrder := range capturedIndex.order {
		if parent != "" {
			if _, exists := currentIndex.nodes[parent]; !exists {
				continue
			}
		}
		var merged []string
		seen := map[string]bool{}
		for _, id := range capturedOrder {
			if _, exists := currentIndex.nodes[id]; !exists || currentIndex.parent[id] != parent {
				continue
			}
			merged = append(merged, id)
			seen[id] = true
		}
		for _, id := range currentIndex.order[parent] {
			if seen[id] || currentIndex.parent[id] != parent {
				continue
			}
			merged = append(merged, id)
			seen[id] = true
		}
		currentIndex.order[parent] = merged
	}

	var build func(string, map[string]bool) ([]Node, error)
	build = func(parent string, ancestors map[string]bool) ([]Node, error) {
		order := append([]string(nil), currentIndex.order[parent]...)
		known := map[string]bool{}
		for _, id := range order {
			known[id] = true
		}
		var missing []string
		for id, candidateParent := range currentIndex.parent {
			if candidateParent == parent && !known[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		order = append(order, missing...)
		result := make([]Node, 0, len(order))
		for _, id := range order {
			if ancestors[id] {
				return nil, fmt.Errorf("topology rebase created a cycle at %s", id)
			}
			node, exists := currentIndex.nodes[id]
			if !exists || currentIndex.parent[id] != parent {
				continue
			}
			nextAncestors := make(map[string]bool, len(ancestors)+1)
			for ancestor := range ancestors {
				nextAncestors[ancestor] = true
			}
			nextAncestors[id] = true
			children, err := build(id, nextAncestors)
			if err != nil {
				return nil, err
			}
			node.Children = children
			result = append(result, node)
		}
		return result, nil
	}
	rebased, err := build("", map[string]bool{})
	if err != nil {
		return nil, err
	}

	// If a stale add targeted a container closed concurrently, preserve its
	// live/pending pane in an explicit recovery tab instead of dropping it.
	present := topologyPaneIDs(rebased)
	for paneID := range topologyPaneIDs(captured) {
		pane := workspace.Panes[paneID]
		if pane == nil || pane.RemovalPending || present[paneID] || !pane.TopologyPending {
			continue
		}
		if len(rebased) == 0 {
			rebased = []Node{{
				ID:   deterministicTopologyID(workspace.ID, "os-window", "recovered"),
				Kind: "os-window",
			}}
		}
		rebased[0].Children = append(rebased[0].Children, Node{
			ID: deterministicTopologyID(workspace.ID, "tab", "recovered/"+pane.ID), Kind: "tab",
			Title: recoveredTabPrefix + shortID(pane.ID), Layout: "splits",
			Children: []Node{{ID: pane.ID, Kind: "pane", PaneID: pane.ID, Title: pane.Title, CWD: pane.CWD}},
		})
		present[paneID] = true
	}
	return rebased, nil
}

func validateTopology(workspace *Workspace, nodes []Node, expected map[string]bool) error {
	if len(nodes) == 0 {
		return fmt.Errorf("topology contains no roots")
	}
	seenNodes := map[string]bool{}
	seenPanes := map[string]bool{}
	var visit func([]Node, string) error
	visit = func(children []Node, parent string) error {
		for _, node := range children {
			if node.ID == "" {
				return fmt.Errorf("topology %s node has no stable id", node.Kind)
			}
			if seenNodes[node.ID] {
				return fmt.Errorf("topology node id %s is duplicated", node.ID)
			}
			seenNodes[node.ID] = true
			switch node.Kind {
			case "os-window":
				if parent != "" {
					return fmt.Errorf("os-window %s is not a root", node.ID)
				}
			case "tab":
				if parent != "os-window" {
					return fmt.Errorf("tab %s is not inside an os-window", node.ID)
				}
			case "pane":
				if parent != "tab" {
					return fmt.Errorf("pane %s is not inside a tab", node.PaneID)
				}
				if node.ID != node.PaneID {
					return fmt.Errorf("pane %s has unstable node id %s", node.PaneID, node.ID)
				}
				if workspace.Panes[node.PaneID] == nil {
					return fmt.Errorf("topology references unknown pane %s", node.PaneID)
				}
				if seenPanes[node.PaneID] {
					return fmt.Errorf("topology contains pane %s more than once", node.PaneID)
				}
				seenPanes[node.PaneID] = true
			default:
				return fmt.Errorf("unsupported topology node kind %q", node.Kind)
			}
			if err := visit(node.Children, node.Kind); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(nodes, ""); err != nil {
		return err
	}
	if !samePaneSet(seenPanes, expected) {
		return fmt.Errorf("topology pane set does not equal active workspace pane set")
	}
	return nil
}

func installDesiredTopology(workspace *Workspace, nodes []Node) (bool, error) {
	nodes, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, nodes)
	if err != nil {
		return false, err
	}
	expected := activeTopologyPaneIDs(workspace)
	for paneID := range topologyPaneIDs(nodes) {
		if pane := workspace.Panes[paneID]; pane != nil && pane.TopologyPending {
			pane.TopologyPending = false
			pane.TopologyPendingAt = time.Time{}
			expected[paneID] = true
		}
	}
	if err := validateTopology(workspace, nodes, expected); err != nil {
		return false, err
	}
	digest := topologyDigest(nodes)
	if digest == workspace.Topology.Digest {
		return false, nil
	}
	workspace.Topology.Generation++
	if workspace.Topology.Generation == 0 {
		workspace.Topology.Generation = 1
	}
	workspace.Topology.Digest = digest
	workspace.Topology.Roots = canonicalTopology(nodes)
	return true, nil
}

func appendRecoveredPane(workspace *Workspace, pane *Pane) (bool, error) {
	nodes := cloneNodes(workspace.Topology.Roots)
	if len(nodes) == 0 {
		nodes = []Node{{
			ID:   deterministicTopologyID(workspace.ID, "os-window", "recovered"),
			Kind: "os-window",
		}}
	}
	if nodes[0].Kind != "os-window" {
		return false, fmt.Errorf("canonical topology has no recovery OS window")
	}
	nodes[0].Children = append(nodes[0].Children, Node{
		ID:     deterministicTopologyID(workspace.ID, "tab", "recovered/"+pane.ID),
		Kind:   "tab",
		Title:  recoveredTabPrefix + shortID(pane.ID),
		Layout: "splits",
		Children: []Node{{
			ID: pane.ID, Kind: "pane", PaneID: pane.ID, Title: pane.Title, CWD: pane.CWD,
		}},
	})
	return installDesiredTopology(workspace, nodes)
}

func recoverMissingTopologyPanes(workspace *Workspace) (bool, error) {
	previousDigest := workspace.Topology.Digest
	base := workspace.Topology.Roots
	if len(base) == 0 {
		base = workspace.Manifest.Topology
	}
	stable, err := stabilizeTopologyIDs(workspace.ID, nil, base)
	if err != nil {
		return false, err
	}
	present := topologyPaneIDs(stable)
	var missing []*Pane
	for _, pane := range workspace.SortedPanes() {
		if pane.RemovalPending || pane.TopologyPending || present[pane.ID] {
			continue
		}
		missing = append(missing, pane)
	}
	if len(stable) == 0 && len(missing) != 0 {
		stable = []Node{{
			ID:   deterministicTopologyID(workspace.ID, "os-window", "recovered"),
			Kind: "os-window",
		}}
	}
	if len(missing) != 0 {
		if len(stable) == 0 {
			return false, fmt.Errorf("cannot recover panes into an empty topology")
		}
		if stable[0].Kind != "os-window" {
			return false, fmt.Errorf("cannot recover panes into non-window topology")
		}
		for _, pane := range missing {
			stable[0].Children = append(stable[0].Children, Node{
				ID:     deterministicTopologyID(workspace.ID, "tab", "recovered/"+pane.ID),
				Kind:   "tab",
				Title:  recoveredTabPrefix + shortID(pane.ID),
				Layout: "splits",
				Children: []Node{{
					ID: pane.ID, Kind: "pane", PaneID: pane.ID,
					Title: pane.Title, CWD: pane.CWD,
				}},
			})
			workspace.Panes[pane.ID].Visible = true
		}
	}
	if len(stable) == 0 {
		return false, nil
	}
	if err := validateTopology(workspace, stable, activeTopologyPaneIDs(workspace)); err != nil {
		return false, err
	}
	workspace.Topology.Roots = canonicalTopology(stable)
	workspace.Topology.Digest = topologyDigest(stable)
	changed := workspace.Topology.Digest != previousDigest
	if workspace.Topology.Generation == 0 {
		workspace.Topology.Generation = 1
	} else if changed {
		workspace.Topology.Generation++
	}
	return changed, nil
}

func annotateRuntimeViews(nodes []Node, views map[string]RuntimeView) {
	for _, osNode := range nodes {
		for _, tabNode := range osNode.Children {
			for _, paneNode := range tabNode.Children {
				view, ok := views[paneNode.PaneID]
				if !ok {
					continue
				}
				view.TabNodeID = tabNode.ID
				view.OSWindowNodeID = osNode.ID
				views[paneNode.PaneID] = view
			}
		}
	}
}

func workspaceLaunchOptions(workspace *Workspace) string {
	type paneOptions struct {
		ID      string
		Options []string
	}
	values := make([]paneOptions, 0, len(workspace.Panes))
	for _, pane := range workspace.Panes {
		values = append(values, paneOptions{ID: pane.ID, Options: pane.LaunchOptions})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
