package zka

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This is the complete lifecycle that the smaller remote tests deliberately
// do not model: the origin sanitises provider-local Kitty identity from an
// allocation, the provider caches that endpoint-less proposal, Kitty creates
// the tagged window, and the provider proves the new topology back to the
// origin. A break at any protocol, cache, admission, or publication boundary
// leaves a proposed pane that the origin will retire after sixty seconds.
func TestRemotePaneLifecycleSurvivesAdmissionDeadline(t *testing.T) {
	tests := []struct {
		name       string
		paneCount  int
		refresh    bool
		kittyRoots func(string, []string) []kittyOSWindow
	}{
		{
			name: "split_in_existing_tab", paneCount: 1,
			kittyRoots: func(workspaceID string, panes []string) []kittyOSWindow {
				return kittyTreeForOSWindows(workspaceID, [][][]string{{panes}})
			},
		},
		{
			name: "new_tab", paneCount: 1,
			kittyRoots: func(workspaceID string, panes []string) []kittyOSWindow {
				return kittyTreeForOSWindows(workspaceID, [][][]string{{{panes[0]}, {panes[1]}}})
			},
		},
		{
			name: "new_os_window", paneCount: 1,
			kittyRoots: func(workspaceID string, panes []string) []kittyOSWindow {
				return kittyTreeForOSWindows(workspaceID, [][][]string{{{panes[0]}}, {{panes[1]}}})
			},
		},
		{
			name: "two_allocations_before_one_capture", paneCount: 2,
			kittyRoots: func(workspaceID string, panes []string) []kittyOSWindow {
				return kittyTreeForOSWindows(workspaceID, [][][]string{{{panes[0], panes[1]}, {panes[2]}}})
			},
		},
		{
			name: "origin_refresh_while_admission_capture_is_blocked", paneCount: 1, refresh: true,
			kittyRoots: func(workspaceID string, panes []string) []kittyOSWindow {
				return kittyTreeForOSWindows(workspaceID, [][][]string{{{panes[0]}, {panes[1]}}})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newRemotePaneLifecycleRig(t)
			workspace, attachment := rig.attachedWorkspace(t)
			initialPane := firstPane(workspace)
			baseGeneration := workspace.Topology.Generation

			paneIDs := []string{initialPane.ID}
			var allocatedIDs []string
			for index := 0; index < test.paneCount; index++ {
				var allocated allocatePaneResponse
				rig.remoteCall(t, "allocate_pane", newRemotePaneAllocation(
					workspace.ID, attachment.ID, fmt.Sprintf("allocation-%d", index), initialPane.ID,
				), &allocated)
				if allocated.Pane == nil {
					t.Fatal("remote allocation returned no pane")
				}
				allocatedIDs = append(allocatedIDs, allocated.Pane.ID)
				paneIDs = append(paneIDs, allocated.Pane.ID)
			}

			// Reproduce the exact dangerous intermediate state: the origin and
			// provider both know the pane, but neither can retain a Unix endpoint
			// received through the remote allocation response. Without the later
			// provider callback, each proposal is eligible for retirement at the
			// deadline even though its Kitty window exists on the provider.
			originBefore, err := rig.origin.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			providerBefore, err := rig.provider.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, paneID := range allocatedIDs {
				originPane := originBefore.Panes[paneID]
				providerPane := providerBefore.Panes[paneID]
				if originPane == nil || !originPane.Proposed() || originPane.Admission.Endpoint != "" {
					t.Fatalf("origin proposal before admission = %#v", originPane)
				}
				if providerPane == nil || !providerPane.Proposed() || providerPane.Admission.Endpoint != "" {
					t.Fatalf("provider proposal before admission = %#v", providerPane)
				}
				if !retirableProposedPane(originPane, false, originPane.PhaseAt.Add(paneAdmissionDeadline+time.Second)) {
					t.Fatalf("test did not reproduce an at-risk endpoint-less proposal: %#v", originPane)
				}
			}

			tree := test.kittyRoots(workspace.ID, paneIDs)
			captureStarted := make(chan struct{})
			releaseCapture := make(chan struct{})
			var captureBlock *remotePaneKittyCaptureBlock
			if test.refresh {
				captureBlock = &remotePaneKittyCaptureBlock{started: captureStarted, release: releaseCapture}
			}
			rig.setKittyTree(workspace.ID, tree, captureBlock)

			// Use the same local daemon RPC issued by runRemotePane after Kitty and
			// zmx become ready. admitPane then crosses the real SSH/yamux test
			// transport to update the authoritative origin.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if test.refresh {
				admitDone := make(chan error, 1)
				go func() {
					_, admitErr := NewAPI(rig.provider.paths).AdmitPane(
						ctx, workspace.ID, allocatedIDs[0], attachment.Endpoint,
					)
					admitDone <- admitErr
				}()
				select {
				case <-captureStarted:
				case <-ctx.Done():
					cancel()
					t.Fatal("admission did not reach its blocked Kitty listing")
				}
				var refreshed Workspace
				rig.remoteCall(t, "get", refRequest{Ref: workspace.ID}, &refreshed)
				providerDuringRefresh, getErr := rig.provider.getWorkspace(workspace.ID)
				if getErr != nil {
					close(releaseCapture)
					cancel()
					t.Fatal(getErr)
				}
				bound := providerDuringRefresh.Panes[allocatedIDs[0]]
				if bound == nil || bound.Admission.Endpoint != attachment.Endpoint || bound.Admission.AttachmentID != attachment.ID {
					close(releaseCapture)
					cancel()
					t.Fatalf("origin refresh erased provider-local admission binding: %#v", bound)
				}
				close(releaseCapture)
				err = <-admitDone
			} else {
				_, err = NewAPI(rig.provider.paths).AdmitPane(ctx, workspace.ID, allocatedIDs[0], attachment.Endpoint)
			}
			cancel()
			if err != nil {
				t.Fatalf("provider admit_pane: %v", err)
			}

			originAfter, err := rig.origin.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			providerAfter, err := rig.provider.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantPanes := make(map[string]bool, len(paneIDs))
			for _, paneID := range paneIDs {
				wantPanes[paneID] = true
			}
			for side, got := range map[string]*Workspace{"origin": originAfter, "provider": providerAfter} {
				if got.Topology.Generation <= baseGeneration {
					t.Fatalf("%s topology generation = %d, want > %d", side, got.Topology.Generation, baseGeneration)
				}
				if !samePaneSet(topologyPaneIDs(got.Topology.Roots), wantPanes) {
					t.Fatalf("%s topology panes = %#v, want %#v", side, topologyPaneIDs(got.Topology.Roots), wantPanes)
				}
				for _, paneID := range allocatedIDs {
					pane := got.Panes[paneID]
					if pane == nil || !pane.Admitted() {
						t.Fatalf("%s pane %s did not survive admission: %#v", side, paneID, pane)
					}
					deadline := pane.PhaseAt.Add(paneAdmissionDeadline + time.Second)
					if retirableProposedPane(pane, false, deadline) {
						t.Fatalf("%s admitted pane %s is still deadline-retirable", side, paneID)
					}
				}
				assertNoFabricatedTabs(t, got)
			}

			observed, err := topologyFromKitty(tree, workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			observed, err = stabilizeTopologyIDs(workspace.ID, originAfter.Topology.Roots, observed)
			if err != nil {
				t.Fatal(err)
			}
			if !topologyMatchesDesired(originAfter, observed) {
				t.Fatal("authoritative topology does not match the provider capture that admitted it")
			}
			for _, call := range rig.kittyRunnerCalls() {
				if strings.Contains(strings.Join(call.Args, " "), "goto_session") {
					t.Fatalf("pane admission rebuilt an already matching Kitty session: %#v", call.Args)
				}
			}
		})
	}
}

// FuzzRemotePaneLifecycle drives the same two-daemon, real-remote-control
// boundary as the regression matrix above. The byte stream chooses a valid
// logical Kitty layout, focus/activity, one to three simultaneous proposals,
// allocation replay, origin refreshes, and whether the provider-local binding
// is raced by an authoritative snapshot. Its seed corpus runs during ordinary
// go test; longer fuzz campaigns explore new event/layout combinations.
func FuzzRemotePaneLifecycle(f *testing.F) {
	for _, seed := range [][]byte{
		{0},
		{1, 1, 1, 1, 1, 1, 1, 1},
		{2, 0, 1, 0, 1, 2, 0, 2, 1, 0, 2},
		{3, 2, 2, 1, 0, 3, 1, 2, 3, 0, 1, 2},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64 {
			input = input[:64]
		}
		stream := topologyFuzzBytes{data: input}
		paneCount := 1 + int(stream.next()%3)
		replayAllocation := stream.next()%2 != 0
		refreshAfterAllocation := stream.next()%2 != 0
		activityDuringProposal := stream.next()%2 != 0
		bindBeforeRefresh := stream.next()%2 != 0

		rig := newRemotePaneLifecycleRig(t)
		workspace, attachment := rig.attachedWorkspace(t)
		initialPane := firstPane(workspace)
		baseGeneration := workspace.Topology.Generation
		paneIDs := []string{initialPane.ID}
		var allocatedIDs []string
		for index := 0; index < paneCount; index++ {
			request := newRemotePaneAllocation(
				workspace.ID, attachment.ID, fmt.Sprintf("fuzz-allocation-%d", index), initialPane.ID,
			)
			var allocated allocatePaneResponse
			rig.remoteCall(t, "allocate_pane", request, &allocated)
			if allocated.Pane == nil {
				t.Fatalf("input=%x: remote allocation returned no pane", input)
			}
			if replayAllocation {
				var replayed allocatePaneResponse
				rig.remoteCall(t, "allocate_pane", request, &replayed)
				if replayed.Pane == nil || replayed.Pane.ID != allocated.Pane.ID {
					t.Fatalf("input=%x: allocation replay changed pane: first=%#v replay=%#v", input, allocated.Pane, replayed.Pane)
				}
			}
			allocatedIDs = append(allocatedIDs, allocated.Pane.ID)
			paneIDs = append(paneIDs, allocated.Pane.ID)
			if refreshAfterAllocation {
				var refreshed Workspace
				rig.remoteCall(t, "get", refRequest{Ref: workspace.ID}, &refreshed)
			}
		}

		wantInitialState := StateUnknown
		if activityDuringProposal {
			updated, err := rig.origin.applyEvent(context.Background(), Event{
				WorkspaceID: workspace.ID, PaneID: initialPane.ID,
				Kind: "user_prompt", Source: "remote-lifecycle-fuzz", TurnID: "turn-1",
			})
			if err != nil {
				t.Fatalf("input=%x: apply activity event: %v", input, err)
			}
			wantInitialState = updated.Panes[initialPane.ID].State
			var refreshed Workspace
			rig.remoteCall(t, "get", refRequest{Ref: workspace.ID}, &refreshed)
		}

		callbackPane := allocatedIDs[int(stream.next())%len(allocatedIDs)]
		if bindBeforeRefresh {
			if err := rig.provider.bindProposedPaneAdmission(workspace.ID, callbackPane, attachment.Endpoint); err != nil {
				t.Fatalf("input=%x: bind provider proposal: %v", input, err)
			}
			var refreshed Workspace
			rig.remoteCall(t, "get", refRequest{Ref: workspace.ID}, &refreshed)
			providerAfterRefresh, err := rig.provider.getWorkspace(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			bound := providerAfterRefresh.Panes[callbackPane]
			if bound == nil || bound.Admission.Endpoint != attachment.Endpoint || bound.Admission.AttachmentID != attachment.ID {
				t.Fatalf("input=%x: refresh erased local admission binding: %#v", input, bound)
			}
		}

		// Generate only valid layouts: panes are permuted, then each boundary is
		// chosen as a split, a new tab, or a new OS window. The Kitty model also
		// varies active/focused windows, tab titles, and layouts while producing
		// realistic runtime ids and layout_state.
		logical := fuzzLogicalTopology(&stream, paneIDs, true)
		kittyModel := newTopologyFuzzKitty(workspace.ID, logical, &stream)
		tree := kittyModel.tree
		rig.setKittyTree(workspace.ID, tree, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := NewAPI(rig.provider.paths).AdmitPane(ctx, workspace.ID, callbackPane, attachment.Endpoint)
		cancel()
		if err != nil {
			t.Fatalf("input=%x: admit remote pane: %v\nlogical=%#v", input, err, logical)
		}

		wantPanes := make(map[string]bool, len(paneIDs))
		for _, paneID := range paneIDs {
			wantPanes[paneID] = true
		}
		originAfter, err := rig.origin.getWorkspace(workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		providerAfter, err := rig.provider.getWorkspace(workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		for side, got := range map[string]*Workspace{"origin": originAfter, "provider": providerAfter} {
			if got.Topology.Generation <= baseGeneration || !samePaneSet(topologyPaneIDs(got.Topology.Roots), wantPanes) {
				t.Fatalf("input=%x: %s did not converge: generation=%d panes=%#v want=%#v\nlogical=%#v",
					input, side, got.Topology.Generation, topologyPaneIDs(got.Topology.Roots), wantPanes, logical)
			}
			if state := got.Panes[initialPane.ID].State; state != wantInitialState {
				t.Fatalf("input=%x: %s lost concurrent activity: got %q want %q", input, side, state, wantInitialState)
			}
			for _, paneID := range allocatedIDs {
				pane := got.Panes[paneID]
				if pane == nil || !pane.Admitted() || retirableProposedPane(pane, false, pane.PhaseAt.Add(paneAdmissionDeadline+time.Second)) {
					t.Fatalf("input=%x: %s pane %s remained deadline-retirable: %#v", input, side, paneID, pane)
				}
			}
			assertNoFabricatedTabs(t, got)
		}
		observed, err := topologyFromKitty(tree, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		observed, err = stabilizeTopologyIDs(workspace.ID, originAfter.Topology.Roots, observed)
		if err != nil {
			t.Fatal(err)
		}
		if !topologyMatchesDesired(originAfter, observed) {
			t.Fatalf("input=%x: authoritative topology differs from admitted Kitty tree\nlogical=%#v", input, logical)
		}
	})
}

type remotePaneLifecycleRig struct {
	host     string
	origin   *Daemon
	provider *Daemon
	kitty    *remotePaneLifecycleKitty
	runner   *fakeRunner
}

type remotePaneKittyCaptureBlock struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

// remotePaneLifecycleKitty stays installed for the entire daemon lifetime.
// Cache refreshes legitimately start asynchronous presentation workers, so
// replacing d.kitty.Runner between scenario steps is itself a data race and
// makes a purported concurrency test untrustworthy.
type remotePaneLifecycleKitty struct {
	t        *testing.T
	provider *Daemon

	mu          sync.RWMutex
	workspaceID string
	tree        []kittyOSWindow
	block       *remotePaneKittyCaptureBlock
}

func (k *remotePaneLifecycleKitty) configure(workspaceID string, tree []kittyOSWindow, block *remotePaneKittyCaptureBlock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.workspaceID = workspaceID
	k.tree = tree
	k.block = block
}

func (k *remotePaneLifecycleKitty) run(_ context.Context, _ string, args ...string) (string, string, error) {
	k.mu.RLock()
	workspaceID := k.workspaceID
	tree := k.tree
	block := k.block
	k.mu.RUnlock()
	joined := strings.Join(args, " ")
	if block != nil && strings.HasSuffix(joined, "ls") {
		block.once.Do(func() {
			close(block.started)
			<-block.release
		})
	}
	if workspaceID == "" {
		switch {
		case strings.HasSuffix(joined, "ls"):
			return "[]", "", nil
		case strings.Contains(joined, "--version"):
			return "kitten 0.47.0", "", nil
		default:
			return "", "", nil
		}
	}
	current, err := k.provider.getWorkspace(workspaceID)
	if err != nil {
		return "", "", err
	}
	return kittyResponse(k.t, args, tree, current)
}

func newRemotePaneLifecycleRig(t *testing.T) *remotePaneLifecycleRig {
	t.Helper()
	const host = "origin.test"
	root := testRoot(t)
	originRoot := filepath.Join(root, "origin")
	providerRoot := filepath.Join(root, "provider")
	origin, err := newTestDaemon(t, originRoot, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	provider, err := newTestDaemon(t, providerRoot, quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	provider.config.SSH.Command = os.Args[0]
	provider.config.SSH.Options = []string{"-test.run=^TestRemoteAttachRegressionSSHHelperProcess$", "--"}
	kitty := &remotePaneLifecycleKitty{t: t, provider: provider}
	runner := &fakeRunner{handler: kitty.run}
	provider.kitty.Runner = runner
	provider.kitty.Command = "kitten-remote-lifecycle"

	originConfigPath := filepath.Join(root, "origin-config.json")
	providerConfigPath := filepath.Join(root, "provider-config.json")
	writeRemoteAttachRegressionConfig(t, originConfigPath, origin.config)
	writeRemoteAttachRegressionConfig(t, providerConfigPath, provider.config)
	t.Setenv("ZKA_CONFIG", providerConfigPath)
	t.Setenv(remoteAttachRegressionHelperEnv, "remote-control")
	t.Setenv(remoteAttachRegressionOriginRootEnv, originRoot)
	t.Setenv(remoteAttachRegressionOriginConfigEnv, originConfigPath)
	serveTestDaemon(t, origin)
	serveTestDaemon(t, provider)
	return &remotePaneLifecycleRig{host: host, origin: origin, provider: provider, kitty: kitty, runner: runner}
}

func (r *remotePaneLifecycleRig) setKittyTree(workspaceID string, tree []kittyOSWindow, block *remotePaneKittyCaptureBlock) {
	r.kitty.configure(workspaceID, tree, block)
}

func (r *remotePaneLifecycleRig) attachedWorkspace(t *testing.T) (*Workspace, *Attachment) {
	t.Helper()
	plan, err := TemplateGenesis(DefaultSessionTemplate(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := r.origin.createWorkspace(createWorkspaceRequest{
		Name: "remote-pane-lifecycle", Shell: []string{"fish"}, Panes: plan.Panes,
		Topology: plan.Topology, FocusPane: plan.FocusPane,
	})
	if err != nil {
		t.Fatal(err)
	}
	pane := firstPane(workspace)
	attachmentID := localAttachmentID(r.provider.state.Node.ID, workspace.ID)
	if _, err := r.origin.registerAttachment(workspace.ID, Attachment{
		ID: attachmentID, Node: r.provider.state.Node,
		Transport: Transport{Kind: "ssh", Host: r.host},
		Endpoint:  "ssh:" + r.provider.state.Node.Name + ":" + attachmentID,
	}); err != nil {
		t.Fatal(err)
	}

	// Fetch through the remote protocol so the provider starts from precisely
	// the projection a real remote attach receives.
	var authoritative Workspace
	r.remoteCall(t, "get", refRequest{Ref: workspace.ID}, &authoritative)
	localEndpoint := "unix:" + filepath.Join(r.provider.paths.AttachmentDir, attachmentID+".sock")
	providerAPI := NewAPI(r.provider.paths)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = providerAPI.RegisterAttachment(ctx, workspace.ID, Attachment{
		ID: attachmentID, Node: r.provider.state.Node,
		Transport: Transport{Kind: "ssh", Host: r.host}, Endpoint: localEndpoint,
	})
	cancel()
	if err != nil {
		t.Fatal(err)
	}

	providerWorkspace, err := r.provider.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachmentID,
		TopologyGeneration: providerWorkspace.Topology.Generation,
		TopologyDigest:     providerWorkspace.Topology.Digest,
		ObservedTopology:   providerWorkspace.Topology.Roots,
		Status:             AttachmentReady,
		Views:              readyView(pane.ID, 11),
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := providerAPI.UpdateAttachment(ctx, ready); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	var originReady Workspace
	r.remoteCall(t, "update_attachment", ready, &originReady)
	if got := originReady.Attachments[attachmentID]; got == nil || got.Status != AttachmentReady {
		t.Fatalf("origin attachment after readiness = %#v", got)
	}
	providerReady, err := r.provider.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	local := providerReady.Attachments[attachmentID]
	if local == nil || local.Endpoint != localEndpoint || local.Status != AttachmentReady {
		t.Fatalf("provider attachment after readiness = %#v", local)
	}
	return providerReady, local
}

func (r *remotePaneLifecycleRig) remoteCall(t *testing.T, op string, payload, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewAPI(r.provider.paths).RemoteCall(ctx, r.host, op, payload, out); err != nil {
		t.Fatalf("remote %s: %v", op, err)
	}
}

func (r *remotePaneLifecycleRig) kittyRunnerCalls() []runnerCall {
	return r.runner.Calls()
}
