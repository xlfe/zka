package zka

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return captureCommand(cmd)
}

// configuredCommandRunner is optional so small test runners only need the
// basic CommandRunner contract. ExecRunner uses it when a process needs the
// exact environment that will become durable inside a zmx backend.
type configuredCommandRunner interface {
	RunConfigured(ctx context.Context, name string, args []string, environment []string, directory string) (stdout string, stderr string, err error)
}

func (ExecRunner) RunConfigured(ctx context.Context, name string, args []string, environment []string, directory string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = environment
	if directory != "" {
		cmd.Dir = directory
	}
	return captureCommand(cmd)
}

func runConfiguredCommand(ctx context.Context, runner CommandRunner, name string, args, environment []string, directory string) (string, string, error) {
	if configured, ok := runner.(configuredCommandRunner); ok {
		return configured.RunConfigured(ctx, name, args, environment, directory)
	}
	return runner.Run(ctx, name, args...)
}

func captureCommand(cmd *exec.Cmd) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
	}
	return stdout.String(), stderr.String(), err
}
