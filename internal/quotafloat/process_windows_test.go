//go:build windows

package quotafloat

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestEnvironmentBlockRoundTripAndWindowsOrdering(t *testing.T) {
	entries := []string{"ZED=value", "Path=C:\\Windows", "=C:=C:\\"}
	block, err := makeEnvironmentBlock(entries)
	if err != nil {
		t.Fatal(err)
	}
	got, err := environmentBlockToSlice(&block[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"=C:=C:\\", "Path=C:\\Windows", "ZED=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment round trip = %#v, want sorted %#v", got, want)
	}
}

func TestEmptyEnvironmentBlockHasTwoNULs(t *testing.T) {
	block, err := makeEnvironmentBlock(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(block, []uint16{0, 0}) {
		t.Fatalf("empty environment block = %#v, want two NULs", block)
	}
}

func TestQuotaFloatPathRequiresExactExecutableName(t *testing.T) {
	if !isQuotaFloatPath(`C:\\Tools\\quota-float.exe`) {
		t.Fatal("expected quota-float.exe to be accepted")
	}
	for _, path := range []string{
		`C:\\Tools\\quota-float-helper.exe`,
		`C:\\Tools\\quota-float.exe.bak`,
		`quota-float.exe`,
	} {
		if isQuotaFloatPath(path) {
			t.Fatalf("accepted non-target process path %q", path)
		}
	}
}

func TestQuotaFloatExecutableNameMatchesFixedUTF16Buffer(t *testing.T) {
	for _, name := range []string{"quota-float.exe", "QUOTA-FLOAT.EXE", "QuOtA-fLoAt.ExE"} {
		encoded, err := windows.UTF16FromString(name)
		if err != nil {
			t.Fatal(err)
		}
		buffer := make([]uint16, 260)
		copy(buffer, encoded)
		if !isQuotaFloatExecutableName(buffer) {
			t.Fatalf("did not match %q", name)
		}
	}
	for _, name := range []string{"quota-float.exe.bak", "quota-float.ex", "quota_float.exe", "quota-float.exe😀"} {
		encoded, err := windows.UTF16FromString(name)
		if err != nil {
			t.Fatal(err)
		}
		buffer := make([]uint16, 260)
		copy(buffer, encoded)
		if isQuotaFloatExecutableName(buffer) {
			t.Fatalf("accepted %q", name)
		}
	}
}

// BenchmarkQuotaFloatExecutableName compares the allocation-free matcher with
// the previous UTF-16-to-string path using a synthetic process-entry buffer.
func BenchmarkQuotaFloatExecutableName(b *testing.B) {
	encoded, err := windows.UTF16FromString(quotaFloatExecutable)
	if err != nil {
		b.Fatal(err)
	}
	buffer := make([]uint16, 260)
	copy(buffer, encoded)

	b.Run("direct_utf16", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !isQuotaFloatExecutableName(buffer) {
				b.Fatal("direct matcher rejected quota-float.exe")
			}
		}
	})
	b.Run("UTF16ToString_EqualFold", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !strings.EqualFold(windows.UTF16ToString(buffer), quotaFloatExecutable) {
				b.Fatal("legacy matcher rejected quota-float.exe")
			}
		}
	})
}

// This test builds a GUI helper in a temporary directory and calls
// restartProcess directly. It never calls listProcesses or Manager, so a
// user's real quota-float process cannot be selected by the test.
func TestRestartProcessWithTemporaryHelper(t *testing.T) {
	t.Parallel()
	testRestartProcessWithTemporaryHelper(t, nil)
}

// The replacement must keep restrictions belonging to the original process,
// even when the caller's own token grants more privileges.
func TestRestartProcessPreservesRestrictedPrivileges(t *testing.T) {
	t.Parallel()
	var current windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_ASSIGN_PRIMARY, &current); err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	var restricted windows.Token
	createRestrictedToken := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	result, _, err := createRestrictedToken.Call(uintptr(current), 1, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&restricted)))
	if result == 0 {
		t.Fatalf("create restricted test token: %v", err)
	}
	defer restricted.Close()
	if err := windows.AdjustTokenPrivileges(restricted, true, nil, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	testRestartProcessWithTemporaryHelper(t, &syscall.SysProcAttr{Token: syscall.Token(restricted)})
}

// Run the test executable elevated to cover BKNetwork's normal UAC startup.
// Only temporary helpers are launched; no installed quota-float is selected.
func TestRestartProcessFromElevatedCaller(t *testing.T) {
	var current windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &current); err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if !current.IsElevated() {
		t.Skip("requires an elevated test runner to exercise UAC")
	}
	limited, err := current.GetLinkedToken()
	if err != nil {
		t.Fatalf("get ordinary UAC test token: %v", err)
	}
	defer limited.Close()
	if limited.IsElevated() {
		t.Fatal("the UAC helper must start without elevation")
	}
	testRestartProcessWithTemporaryHelper(t, &syscall.SysProcAttr{Token: syscall.Token(limited)})
}

func testRestartProcessWithTemporaryHelper(t *testing.T, attributes *syscall.SysProcAttr) {
	t.Helper()
	dir := t.TempDir()
	executable, report := buildQuotaFloatTestHelper(t, dir)
	oldCmd := exec.Command(executable)
	oldCmd.SysProcAttr = attributes
	if err := oldCmd.Start(); err != nil {
		t.Fatalf("start temporary helper: %v", err)
	}
	waited := false
	currentPID := uint32(oldCmd.Process.Pid)
	t.Cleanup(func() {
		if currentPID != 0 {
			terminateTestProcess(currentPID)
		}
		if !waited {
			_ = oldCmd.Process.Kill()
			_, _ = oldCmd.Process.Wait()
		}
	})

	old, err := inspectTestProcess(currentPID)
	if err != nil {
		t.Fatal(err)
	}
	if !waitForReport(t, report, func(content string) bool {
		return strings.HasPrefix(content, strconv.FormatUint(uint64(currentPID), 10)+"\n")
	}) {
		t.Fatal("temporary helper did not publish its initial environment")
	}
	securityBefore := testProcessSecurity(t, currentPID)
	standardProxy := standardProxyValues(t)
	before := append([]string(nil), os.Environ()...)

	// A malformed address fails while preparing the child block. The old
	// process must remain alive because no replacement was created.
	if _, err := restartProcess(old, "127.0.0.1:7897\x00invalid"); err == nil {
		t.Fatal("expected malformed proxy address to fail")
	}
	if !testProcessRunning(currentPID) {
		t.Fatal("original helper exited after replacement creation failed")
	}

	next, err := restartProcess(old, "127.0.0.1:7897")
	if err != nil {
		t.Fatalf("restart temporary helper: %v", err)
	}
	currentPID = next.PID
	if got := testProcessSecurity(t, currentPID); !reflect.DeepEqual(got, securityBefore) {
		t.Fatal("replacement changed the original process token security context")
	}
	if err := oldCmd.Wait(); err != nil {
		// TerminateProcess reports a non-zero exit status by design.
		if oldCmd.ProcessState == nil || oldCmd.ProcessState.Exited() == false {
			t.Fatalf("wait for original helper: %v", err)
		}
	}
	waited = true
	if testProcessRunning(old.PID) {
		t.Fatal("original helper is still running after successful replacement")
	}
	if !waitForReport(t, report, func(content string) bool {
		return strings.HasPrefix(content, strconv.FormatUint(uint64(next.PID), 10)+"\n")
	}) {
		t.Fatal("replacement helper did not publish its environment")
	}
	content, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(content), "\n")
	wantProxy := "http://127.0.0.1:7897"
	if len(lines) < 6 || lines[5] != "true" || lines[1] != wantProxy || lines[2] != wantProxy || lines[3] != wantProxy || lines[4] != "localhost,127.0.0.1,::1" {
		t.Fatalf("replacement environment = %#v", lines)
	}
	if !reflect.DeepEqual(os.Environ(), before) {
		t.Fatal("restart changed BKNetwork's own environment")
	}

	// An empty address must rebuild the original user's standard environment
	// for the next child. This exercises the same path used when the manager
	// disables the existing switch.
	restored, err := restartProcess(next, "")
	if err != nil {
		t.Fatalf("restore temporary helper environment: %v", err)
	}
	currentPID = restored.PID
	if got := testProcessSecurity(t, currentPID); !reflect.DeepEqual(got, securityBefore) {
		t.Fatal("restore changed the original process token security context")
	}
	if testProcessRunning(next.PID) {
		t.Fatal("proxy-enabled helper is still running after restore")
	}
	if !waitForReport(t, report, func(content string) bool {
		return strings.HasPrefix(content, strconv.FormatUint(uint64(restored.PID), 10)+"\n")
	}) {
		t.Fatal("restored helper did not publish its environment")
	}
	content, err = os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(string(content), "\n")
	proxyNames := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}
	if len(lines) < 6 || lines[5] != "true" {
		t.Fatalf("restored environment = %#v", lines)
	}
	for i, name := range proxyNames {
		if lines[i+1] != standardProxy[name] {
			t.Fatalf("restored %s differs from the user standard environment", name)
		}
	}
	if !reflect.DeepEqual(os.Environ(), before) {
		t.Fatal("restore changed BKNetwork's own environment")
	}
}

func standardProxyValues(t *testing.T) map[string]string {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatalf("open test process token: %v", err)
	}
	defer token.Close()
	environment, err := token.Environ(false)
	if err != nil {
		t.Fatalf("read standard user environment: %v", err)
	}
	result := make(map[string]string, 4)
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			name = strings.ToUpper(name)
			if name == "HTTP_PROXY" || name == "HTTPS_PROXY" || name == "ALL_PROXY" || name == "NO_PROXY" {
				result[name] = value
			}
		}
	}
	return result
}

func buildQuotaFloatTestHelper(t *testing.T, dir string) (string, string) {
	t.Helper()
	report := filepath.Join(dir, "environment.txt")
	source := filepath.Join(dir, "helper.go")
	executable := filepath.Join(dir, quotaFloatExecutable)
	program := fmt.Sprintf(`package main

import (
	"os"
	"strconv"
	"strings"
	"time"
	"syscall"
)

func main() {
	current, _ := syscall.GetCurrentProcess()
	var token syscall.Token
	readable := syscall.OpenProcessToken(current, syscall.TOKEN_QUERY, &token) == nil
	if readable { token.Close() }
	values := []string{
		strconv.Itoa(os.Getpid()),
		os.Getenv("HTTP_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("NO_PROXY"),
		strconv.FormatBool(readable),
	}
	_ = os.WriteFile(%s, []byte(strings.Join(values, "\n")), 0600)
	for {
		time.Sleep(time.Second)
	}
}
`, strconv.Quote(report))
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", executable, source)
	build.Env = append(os.Environ(), "GO111MODULE=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build temporary helper: %v\n%s", err, output)
	}
	return executable, report
}

func inspectTestProcess(pid uint32) (Process, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return Process{}, fmt.Errorf("open temporary helper: %w", err)
	}
	defer windows.CloseHandle(handle)
	process, _, _, err := inspectProcessHandle(handle)
	if err != nil {
		return Process{}, fmt.Errorf("inspect temporary helper: %w", err)
	}
	return process, nil
}

func testProcessRunning(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result != windows.WAIT_OBJECT_0
}

func terminateTestProcess(pid uint32) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_ = windows.TerminateProcess(handle, 1)
	_, _ = windows.WaitForSingleObject(handle, 5_000)
}

func waitForReport(t *testing.T, path string, match func(string) bool) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil && match(string(content)) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// Compare meaningful token properties rather than TokenId, which changes when
// Windows creates a new token object even for an identical security context.
type testSecurityContext struct {
	Elevated        bool
	User, Integrity string
	Groups          map[string]uint32
	Privileges      map[windows.LUID]uint32
}

func testProcessSecurity(t *testing.T, pid uint32) testSecurityContext {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	result := testSecurityContext{Elevated: token.IsElevated(), User: user.User.Sid.String(), Groups: map[string]uint32{}, Privileges: map[windows.LUID]uint32{}}
	for _, group := range groups.AllGroups() {
		result.Groups[group.Sid.String()] = group.Attributes
	}
	privilegeData := testTokenInformation(t, token, windows.TokenPrivileges)
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&privilegeData[0]))
	for _, privilege := range privileges.AllPrivileges() {
		result.Privileges[privilege.Luid] = privilege.Attributes
	}
	integrityData := testTokenInformation(t, token, windows.TokenIntegrityLevel)
	result.Integrity = (*windows.Tokenmandatorylabel)(unsafe.Pointer(&integrityData[0])).Label.Sid.String()
	return result
}

func testTokenInformation(t *testing.T, token windows.Token, class uint32) []byte {
	t.Helper()
	var size uint32
	if err := windows.GetTokenInformation(token, class, nil, 0, &size); !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		t.Fatalf("token information size: %v", err)
	}
	if size == 0 || size > 1024*1024 {
		t.Fatal("unexpected token information size")
	}
	data := make([]byte, size)
	if err := windows.GetTokenInformation(token, class, &data[0], size, &size); err != nil {
		t.Fatal(err)
	}
	return data
}
