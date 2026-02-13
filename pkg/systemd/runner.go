package systemd

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

func UnitName(taskID string) string {
	replacer := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	return replacer.Replace("containerd-systemd-" + taskID + ".service")
}

type Runner struct {
	systemdRunPath string
	systemctlPath  string
}

func NewRunner() *Runner {
	return &Runner{systemdRunPath: "systemd-run", systemctlPath: "systemctl"}
}

func (r *Runner) StartUnit(unit string, command []string, workDir string, env map[string]string) error {
	if unit == "" {
		return fmt.Errorf("unit name is required")
	}
	if len(command) == 0 {
		return fmt.Errorf("command is required")
	}
	args := []string{
		"--unit=" + unit,
		"--collect",
		"--property=Type=exec",
		"--property=KillMode=control-group",
		"--property=Restart=no",
		"--property=Description=containerd-systemd task",
	}
	if workDir != "" {
		args = append(args, "--working-directory="+workDir)
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			args = append(args, "--setenv="+key+"="+env[key])
		}
	}
	args = append(args, "--")
	args = append(args, command...)

	cmd := exec.Command(r.systemdRunPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *Runner) StopUnit(unit string) error {
	return r.systemctl("stop", unit)
}

func (r *Runner) ResetFailed(unit string) error {
	return r.systemctl("reset-failed", unit)
}

func (r *Runner) KillUnit(unit string, signal uint32) error {
	return r.systemctl("kill", "--signal", strconv.FormatUint(uint64(signal), 10), unit)
}

func (r *Runner) MainPID(unit string) (uint32, error) {
	if unit == "" {
		return 0, fmt.Errorf("unit name is required")
	}
	cmd := exec.Command(r.systemctlPath, "show", unit, "--property=MainPID", "--value")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("systemctl show failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "0" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse MainPID %q: %w", raw, err)
	}
	return uint32(parsed), nil
}

func (r *Runner) systemctl(args ...string) error {
	cmd := exec.Command(r.systemctlPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
