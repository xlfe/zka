package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xlfe/zka/internal/zka"
)

// Backend supplies the launcher's read-only workspace data and executes the
// existing zka CLI primitives selected by the user.
type Backend interface {
	Workspaces(context.Context, string) ([]*zka.Workspace, error)
	Execute(context.Context, []string) error
}

type commandBackend struct {
	api     zka.API
	command string
}

func newCommandBackend() (Backend, error) {
	paths, err := zka.DefaultPaths()
	if err != nil {
		return nil, err
	}
	command := os.Getenv("ZKA_COMMAND")
	if command == "" {
		command = siblingExecutable("zka")
	}
	return &commandBackend{api: zka.NewAPI(paths), command: command}, nil
}

func (b *commandBackend) Workspaces(ctx context.Context, host string) ([]*zka.Workspace, error) {
	if host == "" {
		return b.api.Workspaces(ctx)
	}
	var workspaces []*zka.Workspace
	if err := b.api.RemoteCall(ctx, host, "list", nil, &workspaces); err != nil {
		return nil, err
	}
	return workspaces, nil
}

func (b *commandBackend) Execute(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, b.command, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := strings.TrimSpace(output.String())
		if detail != "" {
			return errors.New(detail)
		}
		return fmt.Errorf("zka %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func siblingExecutable(name string) string {
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}
