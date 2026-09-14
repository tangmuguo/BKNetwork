package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStopScriptElevatesRecoveryForDesktopUser(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate stop-script test source")
	}
	sourcePath := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "bknetwork-stop")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read stop script: %v", err)
	}

	temporary := t.TempDir()
	binDir := filepath.Join(temporary, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(temporary, "commands.log")
	statePath := filepath.Join(temporary, "service-stopped")
	recoveryStateDir := filepath.Join(temporary, "state")
	systemctlPath := filepath.Join(binDir, "systemctl")
	pkexecPath := filepath.Join(binDir, "pkexec")
	appPath := filepath.Join(binDir, "bknetwork")

	rewritten := string(source)
	for old, replacement := range map[string]string{
		"systemctl_bin=/usr/bin/systemctl":    "systemctl_bin=" + systemctlPath,
		"pkexec_bin=/usr/bin/pkexec":          "pkexec_bin=" + pkexecPath,
		"app_binary=/opt/bknetwork/bknetwork": "app_binary=" + appPath,
		"state_dir=/var/lib/bknetwork":        "state_dir=" + recoveryStateDir,
	} {
		if !strings.Contains(rewritten, old) {
			t.Fatalf("stop script no longer contains %q", old)
		}
		rewritten = strings.Replace(rewritten, old, replacement, 1)
	}
	stopScriptPath := filepath.Join(temporary, "bknetwork-stop")
	writeStopScriptFixture(t, stopScriptPath, rewritten)
	writeStopScriptFixture(t, filepath.Join(binDir, "id"), "#!/bin/sh\nif [ \"${1:-}\" = -u ]; then\n  printf '%s\\n' 1000\n  exit 0\nfi\nexec /usr/bin/id \"$@\"\n")
	writeStopScriptFixture(t, systemctlPath, "#!/bin/sh\ncase \"${1:-}\" in\n  is-active)\n    if [ -f \"$BKNETWORK_STOP_TEST_STATE\" ]; then\n      printf '%s\\n' inactive\n    else\n      : > \"$BKNETWORK_STOP_TEST_STATE\"\n      printf '%s\\n' active\n    fi\n    ;;\n  stop)\n    printf 'systemctl:%s\\n' \"$*\" >> \"$BKNETWORK_STOP_TEST_LOG\"\n    ;;\nesac\n")
	writeStopScriptFixture(t, pkexecPath, "#!/bin/sh\nprintf 'pkexec:%s\\n' \"$*\" >> \"$BKNETWORK_STOP_TEST_LOG\"\nexec \"$@\"\n")
	writeStopScriptFixture(t, appPath, "#!/bin/sh\nprintf 'app:%s\\n' \"$*\" >> \"$BKNETWORK_STOP_TEST_LOG\"\n")
	writeStopScriptFixture(t, filepath.Join(binDir, "notify-send"), "#!/bin/sh\nprintf 'notify:%s\\n' \"$*\" >> \"$BKNETWORK_STOP_TEST_LOG\"\n")

	command := exec.Command("sh", stopScriptPath)
	command.Env = append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"BKNETWORK_STOP_TEST_LOG="+logPath,
		"BKNETWORK_STOP_TEST_STATE="+statePath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stop script failed: %v\n%s", err, output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fixture log: %v", err)
	}
	recoveryInvocation := "pkexec:" + appPath + " recover --state-dir " + recoveryStateDir
	if !strings.Contains(string(log), recoveryInvocation) {
		t.Fatalf("desktop-user recovery was not elevated\nlog:\n%s", log)
	}
}

func writeStopScriptFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
