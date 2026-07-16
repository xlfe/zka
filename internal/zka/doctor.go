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
	origin := fs.String("origin", "", "test an origin through its SSH alias")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("doctor accepts no positional arguments")
	}
	cfg, cfgErr := LoadConfig()
	checks := []doctorCheck{{Name: "config", OK: cfgErr == nil, Detail: doctorDetail(cfgErr, envOr("ZKA_CONFIG", "built-in defaults"))}}
	if cfgErr != nil {
		return writeDoctorResult(checks, *jsonOut, stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	_, err := api.Ping(ctx)
	checks = append(checks, doctorCheck{Name: "daemon", OK: err == nil, Detail: doctorDetail(err, paths.Socket)})
	stateErr := NewStore(paths).Ensure()
	checks = append(checks, doctorCheck{Name: "state-dir", OK: stateErr == nil, Detail: doctorDetail(stateErr, paths.StateDir)})
	commands := []struct{ name, command string }{
		{"kitty", cfg.Kitty.Command}, {"kitten", cfg.Kitty.KittenCommand},
		{"zmx", cfg.ZMX.Command}, {"ssh", cfg.SSH.Command},
		{"codex", "codex"}, {"ntfy-send", cfg.Notifications.NtfyCommand},
	}
	for _, item := range commands {
		path, lookupErr := exec.LookPath(item.command)
		checks = append(checks, doctorCheck{Name: item.name, OK: lookupErr == nil, Detail: doctorDetail(lookupErr, path)})
	}
	watcherExists, watcherErr := configExists(cfg.Kitty.Watcher)
	if watcherErr == nil && !watcherExists {
		watcherErr = fmt.Errorf("not found")
	}
	checks = append(checks, doctorCheck{Name: "kitty-watcher", OK: watcherErr == nil, Detail: doctorDetail(watcherErr, cfg.Kitty.Watcher)})
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
	if *origin != "" {
		var workspaces []*Workspace
		remoteErr := api.RemoteCall(ctx, *origin, "list", nil, &workspaces)
		detail := fmt.Sprintf("%s (%d workspaces)", *origin, len(workspaces))
		checks = append(checks, doctorCheck{Name: "remote-control", OK: remoteErr == nil, Detail: doctorDetail(remoteErr, detail)})
	}
	return writeDoctorResult(checks, *jsonOut, stdout)
}

func writeDoctorResult(checks []doctorCheck, jsonOut bool, stdout io.Writer) (int, error) {
	failed := false
	for _, check := range checks {
		failed = failed || !check.OK
	}
	if jsonOut {
		if err := writeJSON(stdout, checks); err != nil {
			return 1, err
		}
	} else {
		for _, check := range checks {
			status := "ok"
			if !check.OK {
				status = "FAIL"
			}
			fmt.Fprintf(stdout, "%-5s %-16s %s\n", status, check.Name, check.Detail)
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

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
