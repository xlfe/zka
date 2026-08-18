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

func activeTopologyPaneIDs(workspace *Workspace) map[string]bool {
	result := map[string]bool{}
	for id, pane := range workspace.Panes {
		if pane.Admitted() {
			result[id] = true
		}
	}
	return result
}

// desiredPaneIDs and activeTopologyPaneIDs are provably equal whenever a
// desired topology exists -- that is invariant I1, checked on every commit.
func desiredPaneIDs(workspace *Workspace) map[string]bool {
	if len(workspace.Topology.Roots) != 0 {
		return topologyPaneIDs(workspace.Topology.Roots)
	}
	return activeTopologyPaneIDs(workspace)
}

// canonicalTopology normalizes a tree for storage: runtime focus is dropped and
// containers left empty are pruned. An empty container is not representable --
// Kitty silently deletes an intermediate empty tab and fills a trailing one
// with an unmanaged shell, which then breaks capture permanently.
func canonicalTopology(nodes []Node) []Node {
	return canonicalTopologyAt(nodes, true)
}

func canonicalTopologyAt(nodes []Node, roots bool) []Node {
	result := cloneNodes(nodes)
	var normalize func([]Node) []Node
	normalize = func(children []Node) []Node {
		kept := make([]Node, 0, len(children))
		for index := range children {
			node := &children[index]
			node.Active = false
			node.Focused = false
			if node.Kind == "os-window" {
				node.State = ""
			}
			node.LayoutState = stableLayoutState(node.LayoutState)
			node.Children = normalize(node.Children)
			if node.Kind != "pane" && len(node.Children) == 0 {
				continue
			}
			kept = append(kept, *node)
		}
		return kept
	}
	result = normalize(result)
	// Independent OS windows have no commandable order. Store them in stable
	// identity order as well as ignoring their observed ls order in the digest,
	// otherwise compositor churn causes needless state rewrites.
	if roots {
		slices.SortFunc(result, func(left, right Node) int {
			return strings.Compare(left.ID, right.ID)
		})
	}
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

// The convergence digest covers structure only: node identity, parent/child
// membership, and sibling order. Everything else is presentation.
//
// Presentation is still replicated -- it is stored in Roots and written into
// the session file whenever a window is created -- but it must never gate
// convergence, because an attachment cannot be commanded into most of it:
//
//   - LayoutState carries main_bias/biased_map, which are derived from actual
//     window geometry. Two Kitty windows of different sizes legitimately differ
//     forever, and a user dragging a divider would otherwise bump the
//     generation and force every other attachment to rebuild.
//   - Class/Name are fixed when an OS window is created, and goto_session
//     reuses the anchor window, so the first root's class cannot be changed.
//   - EnabledLayouts comes from each process's kitty.conf.
//   - Pane titles are owned by the running program.
//
// Tab Title and Layout are enforceable, so they are pushed when they differ --
// they are simply not part of identity.
func topologyStructuralIdentity(nodes []Node) []Node {
	return topologyStructuralIdentityAt(nodes, true)
}

func topologyStructuralIdentityAt(nodes []Node, roots bool) []Node {
	result := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, Node{
			ID:       node.ID,
			Kind:     node.Kind,
			PaneID:   node.PaneID,
			Children: topologyStructuralIdentityAt(node.Children, false),
		})
	}
	// Kitty exposes no operation for ordering independent OS windows. Its ls
	// order is compositor/runtime state, not a reproducible part of the layout.
	if roots {
		slices.SortFunc(result, func(left, right Node) int {
			return strings.Compare(left.ID, right.ID)
		})
	}
	return result
}

func topologyStructuralDigest(nodes []Node) string {
	encoded, _ := json.Marshal(topologyStructuralIdentity(nodes))
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func topologyStructureEqual(left, right []Node) bool {
	return topologyStructuralDigest(left) == topologyStructuralDigest(right)
}

func topologyDigest(nodes []Node) string { return topologyStructuralDigest(nodes) }

// topologyIdentity is what an attachment stores as its verified baseline.
// Keeping it structural also stops presentation drift from flipping
// attachmentRuntimeEqual, which would bump timestamps and push a remote
// snapshot on every divider drag.
func topologyIdentity(nodes []Node) []Node { return topologyStructuralIdentity(nodes) }

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
	genesis := len(previous) == 0
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
				if genesis && node.ID != "" && !used[node.ID] {
					bestID, bestScore = node.ID, 1<<21
				}
				for _, candidate := range available[node.Kind] {
					if used[candidate.ID] {
						continue
					}
					score := paneOverlap(*node, candidate)
					if paneSignature(candidate) == signature {
						score += 1 << 24
					}
					if node.ID != "" && candidate.ID == node.ID {
						score += 1 << 12
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

	// A pane whose container was closed concurrently is deliberately NOT
	// fabricated back into a synthetic tab. It stays proposed and is admitted
	// against a real tab by its own attachment's next capture. Fabricated nodes
	// carry no enabled_layouts and no layout_state, which Kitty always reports,
	// so they could never converge.
	return rebased, nil
}

func validateTopology(workspace *Workspace, nodes []Node, expected map[string]bool) error {
	if len(nodes) == 0 {
		return fmt.Errorf("%w: topology contains no roots", errTopologyInvalid)
	}
	seenNodes := map[string]bool{}
	seenPanes := map[string]bool{}
	var visit func([]Node, string) error
	visit = func(children []Node, parent string) error {
		for _, node := range children {
			if node.ID == "" {
				return fmt.Errorf("%w: topology %s node has no stable id", errTopologyInvalid, node.Kind)
			}
			if seenNodes[node.ID] {
				return fmt.Errorf("%w: topology node id %s is duplicated", errTopologyInvalid, node.ID)
			}
			seenNodes[node.ID] = true
			switch node.Kind {
			case "os-window":
				if parent != "" {
					return fmt.Errorf("%w: os-window %s is not a root", errTopologyInvalid, node.ID)
				}
			case "tab":
				if parent != "os-window" {
					return fmt.Errorf("%w: tab %s is not inside an os-window", errTopologyInvalid, node.ID)
				}
			case "pane":
				if parent != "tab" {
					return fmt.Errorf("%w: pane %s is not inside a tab", errTopologyInvalid, node.PaneID)
				}
				if node.ID != node.PaneID {
					return fmt.Errorf("%w: pane %s has unstable node id %s", errTopologyInvalid, node.PaneID, node.ID)
				}
				if workspace.Panes[node.PaneID] == nil {
					return fmt.Errorf("%w: topology references unknown pane %s", errTopologyInvalid, node.PaneID)
				}
				if seenPanes[node.PaneID] {
					return fmt.Errorf("%w: topology contains pane %s more than once", errTopologyInvalid, node.PaneID)
				}
				seenPanes[node.PaneID] = true
			default:
				return fmt.Errorf("%w: unsupported topology node kind %q", errTopologyInvalid, node.Kind)
			}
			// A container with no children cannot be expressed in a session
			// file: Kitty drops an intermediate empty tab and gives a trailing
			// one an unmanaged shell, which then fails every later capture.
			if node.Kind != "pane" && len(node.Children) == 0 {
				return fmt.Errorf("%w: %s %s has no children", errTopologyInvalid, node.Kind, node.ID)
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
		return fmt.Errorf("%w: topology pane set does not equal active workspace pane set", errTopologyInvalid)
	}
	return nil
}

// setDesiredTopology is the sole writer of Roots and Digest. Routing every
// mutation through it is what keeps Digest == topologyStructuralDigest(Roots)
// true; the previous code recomputed the digest and then let
// applyCapturedPaneMetadata write titles into Roots afterwards, which could
// leave the stored target unreachable by construction.
func setDesiredTopology(workspace *Workspace, roots []Node) bool {
	canonical := canonicalTopology(roots)
	digest := topologyStructuralDigest(canonical)
	changed := digest != workspace.Topology.Digest
	workspace.Topology.Roots = canonical
	workspace.Topology.Digest = digest
	if workspace.Topology.Generation == 0 {
		workspace.Topology.Generation = 1
	} else if changed {
		workspace.Topology.Generation++
	}
	return changed
}

// installDesiredTopology stabilizes, validates, and only then mutates. Every
// failure path leaves the workspace byte-identical. The previous version
// promoted panes out of their pending state before validating and never rolled
// back, so one rejected capture could leave a pane marked canonical while
// absent from Roots -- a state in which the workspace can never be rendered
// into a session again, i.e. permanently unattachable.
type topologyInstallAuthority string

const (
	topologyInstallSystem      topologyInstallAuthority = "system"
	topologyInstallVerify      topologyInstallAuthority = "verify"
	topologyInstallGenesis     topologyInstallAuthority = "genesis"
	topologyInstallUserCapture topologyInstallAuthority = "user-capture"
	topologyInstallAdmission   topologyInstallAuthority = "pane-admission"
	topologyInstallClosure     topologyInstallAuthority = "pane-closure"
	topologyInstallOperator    topologyInstallAuthority = "operator-adopt"
)

func installDesiredTopology(workspace *Workspace, nodes []Node, authority topologyInstallAuthority) (bool, error) {
	stable, err := stabilizeTopologyIDs(workspace.ID, workspace.Topology.Roots, nodes)
	if err != nil {
		return false, err
	}
	stable = canonicalTopology(stable)
	expected := activeTopologyPaneIDs(workspace)
	var promote []string
	for paneID := range topologyPaneIDs(stable) {
		if pane := workspace.Panes[paneID]; pane != nil && pane.Proposed() {
			expected[paneID] = true
			promote = append(promote, paneID)
		}
	}
	if err := validateTopology(workspace, stable, expected); err != nil {
		return false, err
	}
	structuralChanged := topologyStructuralDigest(stable) != workspace.Topology.Digest
	if structuralChanged {
		switch authority {
		case topologyInstallVerify:
			return false, fmt.Errorf("refusing structural topology publication from reconciliation verification")
		case topologyInstallGenesis:
			if len(workspace.Topology.Roots) != 0 {
				return false, fmt.Errorf("refusing genesis topology over an existing desired layout")
			}
		case topologyInstallAdmission:
			if len(promote) == 0 {
				return false, fmt.Errorf("pane admission did not add a proposed pane")
			}
		case topologyInstallClosure:
			if len(promote) != 0 {
				return false, fmt.Errorf("pane closure cannot admit proposed panes")
			}
		case topologyInstallSystem, topologyInstallUserCapture, topologyInstallOperator:
		default:
			return false, fmt.Errorf("structural topology publication has no authority")
		}
	}
	if !structuralChanged && len(promote) == 0 {
		// Presentation may still have moved; keep Roots current so restores
		// replay the latest titles and split geometry.
		workspace.Topology.Roots = stable
		return false, nil
	}
	// No failure paths beyond this point.
	now := time.Now().UTC()
	sort.Strings(promote)
	for _, paneID := range promote {
		pane := workspace.Panes[paneID]
		pane.Phase, pane.PhaseAt = PaneAdmitted, now
		pane.Admission.MissingSince = time.Time{}
		pane.UpdatedAt = now
	}
	return setDesiredTopology(workspace, stable), nil
}

// reconcileLoadedTopology re-derives pane membership from persisted state at
// daemon start. It replaces a routine that fabricated synthetic "Recovered"
// tabs -- nodes Kitty can never reproduce, which is what wedged convergence in
// the first place.
//
// It never fabricates and never fails a load. A pane missing from the desired
// topology becomes proposed: its Kitty window survives a daemon restart, so the
// next real capture admits it, and if the window is genuinely gone the
// admission worker retires it. A workspace whose stored topology will not
// validate degrades to "re-derive from Kitty" rather than bricking the daemon.
func reconcileLoadedTopology(workspace *Workspace) (changed bool, degraded error) {
	now := time.Now().UTC()
	base := workspace.Topology.Roots
	fromManifest := false
	if len(base) == 0 {
		base = workspace.Manifest.Topology
		fromManifest = true
	}
	if len(base) == 0 {
		return false, nil
	}
	stable := base
	if fromManifest {
		// Only the migration path re-stabilizes. Re-running ID assignment over
		// an already-canonical tree can silently reassign container IDs, which
		// bumps the generation and invalidates every attachment on each start.
		var err error
		if stable, err = stabilizeTopologyIDs(workspace.ID, nil, base); err != nil {
			return false, fmt.Errorf("stabilize workspace %s topology: %w", workspace.ID, err)
		}
	}
	stable = canonicalTopology(stable)

	// Iterate the live map: SortedPanes returns clones, which would silently
	// discard these transitions.
	present := topologyPaneIDs(stable)
	for _, pane := range workspace.Panes {
		if pane.Retiring() || present[pane.ID] || pane.Proposed() {
			continue
		}
		pane.Phase, pane.PhaseAt = PaneProposed, now
		pane.Admission = PaneAdmission{RequestedAt: now}
		changed = true
	}
	if err := validateTopology(workspace, stable, activeTopologyPaneIDs(workspace)); err != nil {
		workspace.Topology.Roots = nil
		workspace.Topology.Digest = ""
		for _, pane := range workspace.Panes {
			if pane.Retiring() {
				continue
			}
			pane.Phase, pane.PhaseAt = PaneProposed, now
			pane.Admission = PaneAdmission{RequestedAt: now}
		}
		return true, fmt.Errorf("workspace %s topology is unusable and will be re-derived from Kitty: %w", workspace.ID, err)
	}
	if fromManifest || !nodesEqual(stable, workspace.Topology.Roots) ||
		workspace.Topology.Digest != topologyStructuralDigest(stable) {
		changed = setDesiredTopology(workspace, stable) || changed
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
		Options launchOptions
	}
	values := make([]paneOptions, 0, len(workspace.Panes))
	for _, pane := range workspace.Panes {
		values = append(values, paneOptions{ID: pane.ID, Options: pane.LaunchOptions})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
