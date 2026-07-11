package zka

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func runDoctor(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	endpoint := fs.String("to", os.Getenv("KITTY_LISTEN_ON"), "kitty remote-control endpoint")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	checks := []doctorCheck{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := NewAPI(paths).Ping(ctx)
	checks = append(checks, doctorCheck{Name: "daemon", OK: err == nil, Detail: doctorDetail(err, paths.Socket)})
	stateErr := NewStore(paths).Ensure()
	checks = append(checks, doctorCheck{Name: "state-dir", OK: stateErr == nil, Detail: doctorDetail(stateErr, paths.StateDir)})
	commands := []struct{ name, command string }{
		{"kitten", envCommand("ZKA_KITTEN_COMMAND", "kitten")},
		{"zmx", envCommand("ZKA_ZMX_COMMAND", "zmx")},
		{"codex", "codex"},
		{"ntfy-send", ntfyCommand()},
	}
	for _, item := range commands {
		path, lookupErr := exec.LookPath(item.command)
		checks = append(checks, doctorCheck{Name: item.name, OK: lookupErr == nil, Detail: doctorDetail(lookupErr, path)})
	}
	if *endpoint == "" {
		checks = append(checks, doctorCheck{Name: "kitty-remote", OK: false, Detail: "KITTY_LISTEN_ON is unset; pass --to"})
	} else {
		kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
		_, listErr := kitty.List(ctx, *endpoint)
		checks = append(checks, doctorCheck{Name: "kitty-remote", OK: listErr == nil, Detail: doctorDetail(listErr, *endpoint)})
	}
	requirements := "/etc/codex/requirements.toml"
	b, requirementsErr := os.ReadFile(requirements)
	hooksOK := requirementsErr == nil && strings.Contains(string(b), "hook codex")
	hooksDetail := requirements
	if requirementsErr != nil {
		hooksDetail = requirementsErr.Error()
	} else if !hooksOK {
		hooksDetail = "managed zka hook not found in " + requirements
	}
	checks = append(checks, doctorCheck{Name: "codex-hooks", OK: hooksOK, Detail: hooksDetail})
	failed := false
	for _, check := range checks {
		failed = failed || !check.OK
	}
	if *jsonOut {
		if err := writeJSON(stdout, checks); err != nil {
			return 1, err
		}
	} else {
		for _, check := range checks {
			status := "ok"
			if !check.OK {
				status = "FAIL"
			}
			fmt.Fprintf(stdout, "%-5s %-14s %s\n", status, check.Name, check.Detail)
		}
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

func doctorDetail(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func ntfyCommand() string {
	return envCommand("ZKA_NTFY_COMMAND", "ntfy-send")
}

func envCommand(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
