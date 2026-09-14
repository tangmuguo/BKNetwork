package appproxy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sampleChatGPTDesktop(execLine string) string {
	return "[Desktop Entry]\n" +
		"Name=ChatGPT\n" +
		"Comment=keep this comment\n" +
		"Exec=" + execLine + "\n" +
		"Icon=chatgpt\n" +
		"Actions=new-window;\n" +
		"\n" +
		"[Desktop Action new-window]\n" +
		"Name=New window\n" +
		"Exec=" + execLine + " --new-window\n"
}

func sampleQuotaDesktop(execLine string) string {
	return "[Desktop Entry]\r\n" +
		"Type=Application\r\n" +
		"Name=Quota Float Ubuntu\r\n" +
		"Exec=" + execLine + "\r\n" +
		"StartupWMClass=quota-float\r\n"
}

func fixtureInstall(t *testing.T, withAutostart bool) (home, applications string, originalChat, originalQuota, fakeChat string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home with spaces")
	applications = filepath.Join(root, "system applications")
	fakeChat = filepath.Join(root, "fake chatgpt")
	chatExec := "/opt/Chat GPT/bin/chatgpt"
	chatExecDesktop := `"` + chatExec + `"`
	writeFixture(t, filepath.Join(applications, "chatgpt.desktop"), sampleChatGPTDesktop(chatExecDesktop+" %U"))
	quotaExec := "/usr/bin/quota-float"
	writeFixture(t, filepath.Join(applications, "Quota Float Ubuntu.desktop"), sampleQuotaDesktop(quotaExec))
	originalChat = readFixture(t, filepath.Join(applications, "chatgpt.desktop"))
	originalQuota = readFixture(t, filepath.Join(applications, "Quota Float Ubuntu.desktop"))
	if withAutostart {
		writeFixture(t, filepath.Join(home, ".config/autostart/chatgpt.desktop"), "[Desktop Entry]\nName=ChatGPT\nExec="+chatExecDesktop+" %U\n")
		writeFixture(t, filepath.Join(home, ".config/autostart/Quota Float Ubuntu.desktop"), "[Desktop Entry]\nName=Quota Float Ubuntu\nExec="+quotaExec+"\n")
	}
	return home, applications, originalChat, originalQuota, fakeChat
}

func TestConfigPathReadAndRejectNonRegularFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	want := filepath.Join(home, ".local/share/bknetwork/app-proxy/config.json")
	if got := ConfigPath(home); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
	writeFixture(t, want, "{\n  \"version\": 1,\n  \"port\": 7897\n}\n")
	config, err := ReadConfig(home)
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if config.Version != 1 || config.Port != 7897 {
		t.Fatalf("ReadConfig() = %+v", config)
	}

	writeFixture(t, want, "{\"version\":2,\"port\":7897}")
	if _, err := ReadConfig(home); err == nil {
		t.Fatal("ReadConfig() accepted an invalid version")
	}
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(want, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(home); err == nil {
		t.Fatal("ReadConfig() accepted a FIFO")
	}
}

func TestInstallTransformsMenusAndOnlyExistingAutostart(t *testing.T) {
	home, applications, originalChat, originalQuota, _ := fixtureInstall(t, true)
	if err := installAtHome(home, 7897, []string{applications}); err != nil {
		t.Fatalf("installAtHome() error = %v", err)
	}
	config, err := ReadConfig(home)
	if err != nil || config.Port != 7897 {
		t.Fatalf("installed config = %+v, error = %v", config, err)
	}
	if got := readFixture(t, filepath.Join(applications, "chatgpt.desktop")); got != originalChat {
		t.Fatal("install modified the system ChatGPT desktop file")
	}
	if got := readFixture(t, filepath.Join(applications, "Quota Float Ubuntu.desktop")); got != originalQuota {
		t.Fatal("install modified the system quota desktop file")
	}

	chatMenu := readFixture(t, filepath.Join(home, ".local/share/applications/chatgpt.desktop"))
	if !strings.Contains(chatMenu, `Exec="`+wrapperPath(home, kindChatGPT)+`" %U`) {
		t.Fatalf("ChatGPT menu did not preserve %%U or use wrapper:\n%s", chatMenu)
	}
	if !strings.Contains(chatMenu, `Exec="`+wrapperPath(home, kindChatGPT)+`" %U --new-window`) {
		t.Fatalf("ChatGPT action did not preserve its arguments:\n%s", chatMenu)
	}
	if !strings.Contains(chatMenu, "Actions=new-window;") || !strings.Contains(chatMenu, "Comment=keep this comment") {
		t.Fatal("ChatGPT menu lost important non-Exec keys")
	}
	quotaMenu := readFixture(t, filepath.Join(home, ".local/share/applications/Quota Float Ubuntu.desktop"))
	if !strings.Contains(quotaMenu, `Exec="`+wrapperPath(home, kindQuota)+`"`) {
		t.Fatalf("quota menu did not use wrapper:\n%s", quotaMenu)
	}
	if !strings.Contains(quotaMenu, "\r\n") {
		t.Fatal("quota menu did not preserve CRLF line endings")
	}
	chatAutostart := readFixture(t, filepath.Join(home, ".config/autostart/chatgpt.desktop"))
	if !strings.Contains(chatAutostart, `Exec="`+wrapperPath(home, kindChatGPT)+`" %U`) {
		t.Fatalf("ChatGPT autostart did not use wrapper:\n%s", chatAutostart)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/autostart/Quota Float Ubuntu.desktop")); err != nil {
		t.Fatalf("existing quota autostart was unexpectedly removed: %v", err)
	}
	for _, kind := range []appKind{kindChatGPT, kindQuota} {
		data := readFixture(t, wrapperPath(home, kind))
		for _, line := range []string{
			"export HTTP_PROXY=\"$proxy\"",
			"export HTTPS_PROXY=\"$proxy\"",
			"export ALL_PROXY=\"$proxy\"",
			"export http_proxy=\"$proxy\"",
			"export https_proxy=\"$proxy\"",
			"export all_proxy=\"$proxy\"",
			"export NO_PROXY='localhost,127.0.0.1,::1'",
			"export no_proxy=\"$NO_PROXY\"",
		} {
			if !strings.Contains(data, line) {
				t.Errorf("%s wrapper missing %q:\n%s", kind, line, data)
			}
		}
		if strings.Contains(data, "bknetwork") || strings.Contains(data, "go run") {
			t.Errorf("%s wrapper depends on BKNetwork source/binary", kind)
		}
	}
	entries, err := os.ReadDir(backupDir(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("install did not retain any backups")
	}
}

func TestRemoveRestoresOriginalsAndPreservesLaterEdits(t *testing.T) {
	home, applications, originalChat, originalQuota, _ := fixtureInstall(t, true)
	if err := installAtHome(home, 7897, []string{applications}); err != nil {
		t.Fatal(err)
	}
	chatMenuPath := filepath.Join(home, ".local/share/applications/chatgpt.desktop")
	quotaMenuPath := filepath.Join(home, ".local/share/applications/Quota Float Ubuntu.desktop")
	chatAutostartPath := filepath.Join(home, ".config/autostart/chatgpt.desktop")
	quotaAutostartPath := filepath.Join(home, ".config/autostart/Quota Float Ubuntu.desktop")
	chatMenuInstalled := readFixture(t, chatMenuPath)
	quotaMenuInstalled := readFixture(t, quotaMenuPath)
	quotaWrapperInstalled := readFixture(t, wrapperPath(home, kindQuota))
	originalChatAutostart := "[Desktop Entry]\nName=ChatGPT\nExec=\"/opt/Chat GPT/bin/chatgpt\" %U\n"
	if err := os.WriteFile(chatMenuPath, append([]byte(readFixture(t, chatMenuPath)), []byte("# user edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath(home, kindQuota), append([]byte(readFixture(t, wrapperPath(home, kindQuota))), []byte("# user edit\n")...), 0o700); err != nil {
		t.Fatal(err)
	}
	warnings, err := removeAtHome(home, []string{applications})
	if err == nil {
		t.Fatal("removeAtHome() succeeded despite user edits")
	}
	if len(warnings) < 2 {
		t.Fatalf("removeAtHome() warnings = %v, want edits to be detected", warnings)
	}
	if !strings.Contains(readFixture(t, chatMenuPath), "# user edit") {
		t.Fatal("remove overwrote a user-edited desktop file")
	}
	if !strings.Contains(readFixture(t, wrapperPath(home, kindQuota)), "# user edit") {
		t.Fatal("remove overwrote a user-edited wrapper")
	}
	if got := readFixture(t, quotaMenuPath); got != quotaMenuInstalled {
		t.Fatal("remove partially changed a generated quota menu")
	}
	if got := readFixture(t, chatAutostartPath); !strings.Contains(got, `Exec="`+wrapperPath(home, kindChatGPT)+`" %U`) {
		t.Fatal("remove partially restored ChatGPT autostart")
	}
	if got := readFixture(t, quotaAutostartPath); !strings.Contains(got, `Exec="`+wrapperPath(home, kindQuota)+`"`) {
		t.Fatal("remove partially restored quota autostart")
	}
	if _, err := ReadConfig(home); err != nil {
		t.Fatalf("remove disabled config after conflict: %v", err)
	}
	if _, err := os.Stat(journalPath(home)); err != nil {
		t.Fatalf("remove removed active journal after conflict: %v", err)
	}

	// Resolve the edits.  The ChatGPT autostart is deliberately written back
	// to its original bytes before retrying, exercising the already-restored
	// path rather than treating it as a conflict.
	if err := os.WriteFile(chatMenuPath, []byte(chatMenuInstalled), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath(home, kindQuota), []byte(quotaWrapperInstalled), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatAutostartPath, []byte(originalChatAutostart), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, err = removeAtHome(home, []string{applications})
	if err != nil {
		t.Fatalf("removeAtHome() retry error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("removeAtHome() retry warnings = %v", warnings)
	}
	if _, err := os.Stat(quotaMenuPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated quota menu remains after retry, error = %v", err)
	}
	if got := readFixture(t, chatAutostartPath); got != originalChatAutostart {
		t.Fatalf("ChatGPT autostart was not left at original bytes:\n%s", got)
	}
	if got := readFixture(t, quotaAutostartPath); got != "[Desktop Entry]\nName=Quota Float Ubuntu\nExec=/usr/bin/quota-float\n" {
		t.Fatalf("quota autostart was not restored:\n%s", got)
	}
	if got := readFixture(t, filepath.Join(applications, "chatgpt.desktop")); got != originalChat {
		t.Fatal("system ChatGPT desktop changed after remove")
	}
	if got := readFixture(t, filepath.Join(applications, "Quota Float Ubuntu.desktop")); got != originalQuota {
		t.Fatal("system quota desktop changed after remove")
	}
	if _, err := ReadConfig(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config remains enabled after remove: %v", err)
	}
	if _, err := os.Stat(journalPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active journal remains after remove: %v", err)
	}
	if _, err := filepath.Glob(filepath.Join(backupDir(home), "*")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAllowsDeletedAutostart(t *testing.T) {
	home, applications, _, _, _ := fixtureInstall(t, true)
	if err := installAtHome(home, 7897, []string{applications}); err != nil {
		t.Fatal(err)
	}
	quotaAutostartPath := filepath.Join(home, ".config/autostart/Quota Float Ubuntu.desktop")
	if err := os.Remove(quotaAutostartPath); err != nil {
		t.Fatal(err)
	}
	if warnings, err := removeAtHome(home, []string{applications}); err != nil {
		t.Fatalf("removeAtHome() error after autostart deletion = %v (warnings: %v)", err, warnings)
	}
	if _, err := os.Stat(quotaAutostartPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted quota autostart was recreated, error = %v", err)
	}
}

func TestWrapperLaunchExportsProxyAndPreservesArgs(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with spaces")
	root := t.TempDir()
	applications := filepath.Join(root, "applications")
	target := filepath.Join(root, "mock quota dir", "quota-float")
	capture := filepath.Join(root, "capture")
	writeFixture(t, target, "#!/bin/sh\nprintf '%s\\n' \"$HTTP_PROXY\" \"$HTTPS_PROXY\" \"$ALL_PROXY\" \"$http_proxy\" \"$https_proxy\" \"$all_proxy\" \"$NO_PROXY\" \"$no_proxy\" > \"$CAPTURE\"\nprintf '<%s>\\n' \"$@\" >> \"$CAPTURE\"\n")
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(applications, "quota-float.desktop"), sampleQuotaDesktop(`"`+target+`"`+" --from-desktop"))
	if err := installAtHome(home, 7897, []string{applications}); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapperPath(home, kindQuota), "arg with spaces", "%U")
	command.Env = append(os.Environ(), "CAPTURE="+capture)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("quota wrapper failed: %v (%s)", err, output)
	}
	lines := strings.Split(strings.TrimSpace(readFixture(t, capture)), "\n")
	if len(lines) != 10 {
		t.Fatalf("captured wrapper output = %q", lines)
	}
	for index, want := range []string{
		"http://127.0.0.1:7897",
		"http://127.0.0.1:7897",
		"http://127.0.0.1:7897",
		"http://127.0.0.1:7897",
		"http://127.0.0.1:7897",
		"http://127.0.0.1:7897",
		"localhost,127.0.0.1,::1",
		"localhost,127.0.0.1,::1",
	} {
		if lines[index] != want {
			t.Errorf("captured env %d = %q, want %q", index, lines[index], want)
		}
	}
	if lines[8] != "<arg with spaces>" || lines[9] != "<%U>" {
		t.Fatalf("captured args = %q", lines[8:])
	}
}

func TestChatGPTWrapperAddsProxyAndLoopbackBypass(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	applications := filepath.Join(t.TempDir(), "applications")
	target := filepath.Join(t.TempDir(), "chatgpt")
	capture := filepath.Join(t.TempDir(), "capture")
	writeFixture(t, target, "#!/bin/sh\nprintf '<%s>\\n' \"$@\" > \"$CAPTURE\"\n")
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(applications, "chatgpt.desktop"), sampleChatGPTDesktop(target+" %U"))
	if err := installAtHome(home, 7897, []string{applications}); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapperPath(home, kindChatGPT), "arg")
	command.Env = append(os.Environ(), "CAPTURE="+capture)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ChatGPT wrapper failed: %v (%s)", err, output)
	}
	got := readFixture(t, capture)
	if !strings.Contains(got, "<--proxy-server=http://127.0.0.1:7897>") || !strings.Contains(got, "<--proxy-bypass-list=localhost;127.0.0.1;[::1]>") || !strings.Contains(got, "<arg>") {
		t.Fatalf("ChatGPT wrapper args = %q", got)
	}
}
