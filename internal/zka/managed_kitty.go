package zka

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func localAttachmentID(nodeID, workspaceID string) string {
	sum := sha256.Sum256([]byte(nodeID + "\x00" + workspaceID))
	return hex.EncodeToString(sum[:12])
}

func attachmentEndpoint(paths Paths, attachmentID string) string {
	return "unix:" + filepath.Join(paths.AttachmentDir, attachmentID+".sock")
}

func templatePaneSpecs(template SessionTemplate, defaultCWD string) ([]PaneSpec, error) {
	result := make([]PaneSpec, 0, template.Launches)
	for _, line := range template.Lines {
		if !line.Launch {
			continue
		}
		options, _, err := parseLaunch(line.Tokens[1:])
		if err != nil {
			return nil, err
		}
		cwd := plainLaunchOption(options, "--cwd")
		if cwd == "" {
			cwd = defaultCWD
		}
		title := plainLaunchOption(options, "--title")
		if title == "" {
			title = plainLaunchOption(options, "--window-title")
		}
		result = append(result, PaneSpec{CWD: cwd, Title: title})
	}
	return result, nil
}

func plainLaunchOption(options []string, wanted string) string {
	for i := 0; i < len(options); i++ {
		name, value, inline := optionParts(options[i])
		if !inline && launchValueOptions[name] && i+1 < len(options) {
			value = options[i+1]
			i++
		}
		if name == wanted {
			return value
		}
	}
	return ""
}

var kittyValueOptions = map[string]bool{
	"--class": true, "--name": true, "--title": true, "--config": true,
	"--override": true, "-o": true, "--directory": true, "--start-as": true,
	"--listen-on": true, "--session": true, "--watcher": true,
}

func validateKittyPassthrough(args []string) error {
	reserved := map[string]bool{
		"--listen-on": true, "--session": true, "--watcher": true,
		"--single-instance": true, "-1": true, "--detach": true, "-d": true,
	}
	for i := 0; i < len(args); i++ {
		token := args[i]
		name, value, inline := optionParts(token)
		if reserved[name] {
			return fmt.Errorf("kitty option %s is managed by zka", name)
		}
		if name == "--override" || name == "-o" {
			if !inline {
				if i+1 >= len(args) {
					return fmt.Errorf("kitty option %s requires a value", name)
				}
				value = args[i+1]
				i++
			}
			key := strings.SplitN(value, "=", 2)[0]
			if key == "shell" || key == "allow_remote_control" {
				return fmt.Errorf("kitty setting %s is managed by zka", key)
			}
			continue
		}
		if !strings.HasPrefix(token, "-") {
			return fmt.Errorf("kitty program arguments are not supported: %q", token)
		}
		if kittyValueOptions[name] && !inline {
			if i+1 >= len(args) {
				return fmt.Errorf("kitty option %s requires a value", name)
			}
			i++
		}
	}
	return nil
}

type launchAttachmentOptions struct {
	Workspace  *Workspace
	Attachment Attachment
	Session    string
	KittyArgs  []string
}

func launchManagedKitty(ctx context.Context, paths Paths, cfg Config, api API, opts launchAttachmentOptions) (*Workspace, error) {
	if err := validateKittyPassthrough(cfg.Kitty.ExtraArgs); err != nil {
		return nil, fmt.Errorf("configured kitty.extra_args: %w", err)
	}
	if err := validateKittyPassthrough(opts.KittyArgs); err != nil {
		return nil, err
	}
	if exists, err := configExists(cfg.Kitty.Watcher); err != nil {
		return nil, fmt.Errorf("inspect Kitty watcher: %w", err)
	} else if !exists {
		return nil, fmt.Errorf("Kitty watcher not found at %s", cfg.Kitty.Watcher)
	}
	attachment, err := api.RegisterAttachment(ctx, opts.Workspace.ID, opts.Attachment)
	if err != nil {
		return nil, err
	}
	path, err := NewStore(paths).WriteSession(opts.Workspace.ID, attachment.ID, opts.Session)
	if err != nil {
		_, _ = api.DetachAttachment(context.Background(), opts.Workspace.ID, attachment.ID)
		return nil, err
	}
	args := append([]string(nil), cfg.Kitty.ExtraArgs...)
	args = append(args, opts.KittyArgs...)
	managedShell := "zka pane --workspace " + opts.Workspace.ID
	if attachment.Transport.Kind == "ssh" {
		managedShell = "zka remote-new-pane --origin " + attachment.Transport.Host +
			" --workspace " + opts.Workspace.ID + " --attachment " + attachment.ID
	}
	args = append(args, managedKittyOverrides(managedShell)...)
	args = append(args,
		"--listen-on", attachment.Endpoint,
		"--watcher", cfg.Kitty.Watcher,
		"--session", path,
	)
	cmd := exec.Command(cfg.Kitty.Command, args...)
	cmd.Env = append(os.Environ(),
		"ZKA_WATCHER_SOCKET="+paths.WatcherSocket,
		"ZKA_WORKSPACE_ID="+opts.Workspace.ID,
		"ZKA_ATTACHMENT_ID="+attachment.ID,
		"KITTY_LISTEN_ON="+attachment.Endpoint,
	)
	if err := cmd.Start(); err != nil {
		_, _ = api.DetachAttachment(context.Background(), opts.Workspace.ID, attachment.ID)
		return nil, fmt.Errorf("start managed Kitty: %w", err)
	}
	attachment.PID = cmd.Process.Pid
	attachment, err = api.RegisterAttachment(ctx, opts.Workspace.ID, *attachment)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	workspace, err := waitForAttachmentReady(ctx, api, kitty, opts.Workspace, attachment)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_, _ = api.DetachAttachment(context.Background(), opts.Workspace.ID, attachment.ID)
		return nil, err
	}
	if err := cmd.Process.Release(); err != nil {
		return nil, fmt.Errorf("release managed Kitty process: %w", err)
	}
	return workspace, nil
}

func managedKittyOverrides(managedShell string) []string {
	return []string{
		"--override", "allow_remote_control=socket-only",
		"--override", "shell=" + managedShell,
		"--override", "action_alias new_tab_with_cwd launch --type=tab --cwd=last_reported",
		"--override", "action_alias new_window_with_cwd launch --type=window --cwd=last_reported",
		"--override", "action_alias new_os_window_with_cwd launch --type=os-window --cwd=last_reported",
	}
}

func waitForAttachmentReady(ctx context.Context, api API, kitty KittyClient, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	var validationErr error
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		fresh, refreshErr := api.Workspace(callCtx, workspace.ID)
		if refreshErr == nil {
			workspace = fresh
			if current := fresh.Attachments[attachment.ID]; current != nil {
				attachment = current
			}
		}
		var manifest Manifest
		var views map[string]RuntimeView
		err := refreshErr
		if err == nil {
			manifest, views, err = CaptureManifest(callCtx, kitty, attachment.Endpoint, workspace)
		}
		cancel()
		if err == nil {
			if attachment.Role == AttachmentPrimary && workspace.PrimaryAttachmentID == attachment.ID {
				updated, updateErr := api.UpdateManifest(ctx, manifestUpdateRequest{
					Workspace: workspace.ID, Attachment: attachment.ID,
					ExpectedRevision: workspace.Revision, Manifest: manifest, Views: views,
				})
				if updateErr == nil {
					return updated, nil
				}
				lastErr, validationErr = updateErr, updateErr
			} else {
				updated, updateErr := api.UpdateAttachment(ctx, attachmentUpdateRequest{
					Workspace: workspace.ID, Attachment: attachment.ID,
					ExpectedRevision: workspace.Revision, Status: AttachmentReady, Views: views,
				})
				if updateErr == nil {
					return updated, nil
				}
				lastErr, validationErr = updateErr, updateErr
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if validationErr != nil {
		lastErr = validationErr
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return nil, fmt.Errorf("managed Kitty did not become ready: %w", lastErr)
}
