package linuxnet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner is the small command boundary used by Manager.  Keeping commands
// behind this interface makes the state machine testable without ever
// changing the host network.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, safeCommandPath(name), args...)
	// NetworkManager, warp-cli, and ip output is parsed by the manager.  A
	// stable C locale also prevents a user's shell environment from changing
	// status semantics.  The fixed PATH avoids accidentally executing a helper
	// from a user-writable virtualenv.
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "LC_ALL=") || strings.HasPrefix(value, "LANG=") || strings.HasPrefix(value, "PATH=") {
			continue
		}
		env = append(env, value)
	}
	cmd.Env = append(env,
		"LC_ALL=C",
		"LANG=C",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func safeCommandPath(name string) string {
	if strings.ContainsRune(name, '/') {
		return name
	}
	candidates, _ := safeCommandCandidates(filepath.Base(name))
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name
}

type pathFinder interface {
	LookPath(file string) (string, error)
}

type osPathFinder struct{}

func (osPathFinder) LookPath(file string) (string, error) {
	if candidates, ok := safeCommandCandidates(filepath.Base(file)); ok && !strings.ContainsRune(file, '/') {
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return candidate, nil
			}
		}
		return "", exec.ErrNotFound
	}
	return exec.LookPath(file)
}

func safeCommandCandidates(name string) ([]string, bool) {
	known := map[string][]string{
		"nmcli":       {"/usr/bin/nmcli", "/usr/sbin/nmcli"},
		"ip":          {"/usr/sbin/ip", "/usr/bin/ip"},
		"warp-cli":    {"/usr/bin/warp-cli", "/usr/local/bin/warp-cli"},
		"wg":          {"/usr/bin/wg", "/usr/local/bin/wg"},
		"wg-quick":    {"/usr/bin/wg-quick", "/usr/local/bin/wg-quick"},
		"curl":        {"/usr/bin/curl", "/usr/local/bin/curl"},
		"resolvectl":  {"/usr/bin/resolvectl", "/usr/sbin/resolvectl"},
		"resolvconf":  {"/usr/sbin/resolvconf", "/usr/bin/resolvconf"},
		"loginctl":    {"/usr/bin/loginctl"},
		"runuser":     {"/usr/sbin/runuser", "/usr/bin/runuser"},
		"systemd-run": {"/usr/bin/systemd-run"},
		"gsettings":   {"/usr/bin/gsettings"},
	}
	values, ok := known[name]
	return values, ok
}
