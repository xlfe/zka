package zka

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return captureCommand(cmd)
}

type commandOptions struct {
	Environment []string
	Directory   string
	NewSession  bool
}

// configuredCommandRunner is required whenever correctness depends on the
// exact child environment. Falling back to Run would silently inherit zkad's
// environment, which is precisely what provider commands must avoid.
type configuredCommandRunner interface {
	RunConfigured(ctx context.Context, name string, args []string, options commandOptions) (stdout string, stderr string, err error)
}

func (ExecRunner) RunConfigured(ctx context.Context, name string, args []string, options commandOptions) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append([]string(nil), options.Environment...)
	if options.Directory != "" {
		cmd.Dir = options.Directory
	}
	if options.NewSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	return captureCommand(cmd)
}

func runConfiguredCommand(ctx context.Context, runner CommandRunner, name string, args, environment []string, directory string) (string, string, error) {
	return runCommandWithOptions(ctx, runner, name, args, commandOptions{
		Environment: environment,
		Directory:   directory,
	})
}

func runCommandWithOptions(ctx context.Context, runner CommandRunner, name string, args []string, options commandOptions) (string, string, error) {
	configured, ok := runner.(configuredCommandRunner)
	if !ok {
		return "", "", fmt.Errorf("command runner %T cannot preserve an exact process environment", runner)
	}
	options.Environment = append([]string(nil), options.Environment...)
	return configured.RunConfigured(ctx, name, append([]string(nil), args...), options)
}

var _ configuredCommandRunner = ExecRunner{}

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
