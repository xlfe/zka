package zka

import "strings"

// launchSpec is everything needed to describe one zka-managed pane launch.
type launchSpec struct {
	Workspace          *Workspace
	Pane               *Pane
	Transport          Transport
	AttachmentID       string
	OSWindowNodeID     string
	TabNodeID          string
	SerializedWindowID int64
}

// buildLaunch is the single definition of what a zka-managed launch is. The
// session renderer and the `kitten @ launch` argv builder both consume it, so
// they cannot drift apart the way the two hand-written copies had already
// started to.
func buildLaunch(spec launchSpec) LaunchLine {
	options := stripManagedOptions(spec.Pane.LaunchOptions)
	ssh := spec.Transport.Kind == "ssh"
	if ssh {
		options = options.Drop("--cwd")
	} else if !options.Has("--cwd") && spec.Pane.CWD != "" {
		options = append(options, LaunchOption{Name: "--cwd", Value: spec.Pane.CWD})
	}
	if !options.Has("--title") && !options.Has("--window-title") && spec.Pane.Title != "" {
		options = append(options, LaunchOption{Name: "--title", Value: spec.Pane.Title})
	}
	options = append(options,
		LaunchOption{Name: "--var", Value: "zka_workspace=" + spec.Workspace.ID},
		LaunchOption{Name: "--var", Value: "zka_pane=" + spec.Pane.ID},
		LaunchOption{Name: "--var", Value: "zka_state=" + string(spec.Pane.State)},
		LaunchOption{Name: "--var", Value: "zka_ready=0"},
		LaunchOption{Name: "--env", Value: "ZKA_WORKSPACE_ID=" + spec.Workspace.ID},
		LaunchOption{Name: "--env", Value: "ZKA_PANE_ID=" + spec.Pane.ID},
	)
	if spec.OSWindowNodeID != "" {
		options = append(options, LaunchOption{Name: "--var", Value: "zka_os_window=" + spec.OSWindowNodeID})
	}
	if spec.TabNodeID != "" {
		options = append(options, LaunchOption{Name: "--var", Value: "zka_tab=" + spec.TabNodeID})
	}
	args := []string{"zka", "pane", "--workspace", spec.Workspace.ID, "--pane", spec.Pane.ID}
	if ssh {
		args = []string{"zka", "remote-pane", "--origin", spec.Transport.Host,
			"--workspace", spec.Workspace.ID, "--pane", spec.Pane.ID, "--attachment", spec.AttachmentID}
	}
	return LaunchLine{SerializedWindowID: spec.SerializedWindowID, Options: options, Args: args}
}

// stripManagedOptions removes every option zka owns, plus anything outside the
// topology-safe allowlist, so a captured launch can be replayed without
// carrying attachment-local or privileged settings across.
func stripManagedOptions(options launchOptions) launchOptions {
	clean := make(launchOptions, 0, len(options))
	for _, option := range options {
		if !safeTopologyValueOptions[option.Name] && !safeTopologyFlagOptions[option.Name] {
			continue
		}
		if option.Name == "--var" || option.Name == "--env" {
			key := option.Value
			if at := strings.IndexByte(key, '='); at >= 0 {
				key = key[:at]
			}
			if isManagedPaneVariable(key) {
				continue
			}
		}
		if option.Name == "--title" || option.Name == "--window-title" {
			option.Value = stripStateMarker(option.Value)
		}
		clean = append(clean, option)
	}
	return clean
}
