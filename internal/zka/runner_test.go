package zka

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecRunnerConfiguredCommandStartsNewSessionWithExactEnvironment(t *testing.T) {
	stdout, stderr, err := (ExecRunner{}).RunConfigured(context.Background(), os.Args[0], []string{
		"-test.run=TestExecRunnerNewSessionHelper",
	}, commandOptions{
		Environment: []string{"ZKA_TEST_NEW_SESSION=1"},
		NewSession:  true,
	})
	if err != nil {
		t.Fatalf("configured command: %v: %s", err, stderr)
	}
	var pid, session int
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "ZKA-SESSION ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 {
			pid, _ = strconv.Atoi(fields[1])
			session, _ = strconv.Atoi(fields[2])
		}
	}
	if pid == 0 || pid != session {
		t.Fatalf("child pid/session = %d/%d; output=%q", pid, session, stdout)
	}
}

func TestExecRunnerNewSessionHelper(t *testing.T) {
	if os.Getenv("ZKA_TEST_NEW_SESSION") == "" {
		return
	}
	if os.Getenv("PATH") != "" {
		t.Fatal("configured child inherited PATH outside its exact environment")
	}
	session, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("ZKA-SESSION %d %d\n", os.Getpid(), session)
}

type basicOnlyRunner struct{}

func (basicOnlyRunner) Run(context.Context, string, ...string) (string, string, error) {
	return "unexpected", "", nil
}

func TestConfiguredCommandRefusesEnvironmentDroppingFallback(t *testing.T) {
	_, _, err := runCommandWithOptions(context.Background(), basicOnlyRunner{}, "ignored", nil, commandOptions{Environment: []string{"ONLY=this"}})
	if err == nil || !strings.Contains(err.Error(), "cannot preserve an exact process environment") {
		t.Fatalf("configured runner error = %v", err)
	}
}
